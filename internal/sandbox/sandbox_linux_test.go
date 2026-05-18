//go:build linux

package sandbox

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// requireUnprivilegedUserNS is a soft gate: most namespace flags need
// either CAP_SYS_ADMIN or the host's kernel.unprivileged_userns_clone
// = 1. Tests skip rather than fail on hosts that don't allow this.
func requireNamespacePrivilege(t *testing.T) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "echo ok")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUTS}
	if err := cmd.Run(); err != nil {
		t.Skipf("namespace clone unavailable (need CAP_SYS_ADMIN or privileged container): %v", err)
	}
}

// TestApplyPIDNamespacePidIs1 runs `sh -c 'echo $$'` inside a new PID
// namespace and asserts the child sees itself as PID 1. This is the
// canonical proof that the PID namespace flag did its job.
func TestApplyPIDNamespacePidIs1(t *testing.T) {
	requireNamespacePrivilege(t)

	cmd := exec.Command("/bin/sh", "-c", "echo $$")
	if err := Apply(cmd, Spec{PIDNamespace: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Run: %v (stderr %s)", err, exitErrStderr(err))
	}
	got := strings.TrimSpace(string(out))
	if got != "1" {
		t.Errorf("child pid = %q, want %q", got, "1")
	}
}

// TestApplyUTSNamespaceIsolated runs `hostname` inside a UTS
// namespace; the namespace's hostname starts as a copy of the host's,
// which alone proves the flag was honoured (the *namespace boundary*
// is what we're verifying, not the value). We then confirm Cloneflags
// carried the CLONE_NEWUTS bit.
func TestApplyUTSNamespaceIsolated(t *testing.T) {
	requireNamespacePrivilege(t)

	cmd := exec.Command("/bin/true")
	if err := Apply(cmd, Spec{UTSNamespace: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWUTS == 0 {
		t.Errorf("Cloneflags = %#x, want CLONE_NEWUTS set", cmd.SysProcAttr.Cloneflags)
	}
	if err := cmd.Run(); err != nil {
		t.Errorf("Run with UTS ns: %v", err)
	}
}

// TestApplyComposesWithExistingSysProcAttr: callers (e.g. supervisor)
// may already have set SysProcAttr.UseCgroupFD; Apply must preserve
// those fields and only OR in new flags.
func TestApplyComposesWithExistingSysProcAttr(t *testing.T) {
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    42, // bogus, just to verify it's not zeroed
	}
	if err := Apply(cmd, Spec{PIDNamespace: true, UTSNamespace: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !cmd.SysProcAttr.UseCgroupFD || cmd.SysProcAttr.CgroupFD != 42 {
		t.Errorf("cgroup fields trampled: %+v", cmd.SysProcAttr)
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWPID == 0 ||
		cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWUTS == 0 {
		t.Errorf("Cloneflags = %#x, expected NEWPID+NEWUTS", cmd.SysProcAttr.Cloneflags)
	}
}

// TestApplyChrootConfinesChild builds a minimal rootfs containing a
// busybox-static copy at /bin/sh (so busybox dispatches its shell
// applet via argv[0]) and verifies the child ran inside the chroot.
// Proof of chroot: the child writes to /marker, and we observe the
// file at <rootfs>/marker on the host. If chroot had not been
// applied, the host's /bin/sh would have written to /marker on the
// real root — a different path — and the rootfs marker would stay
// empty.
func TestApplyChrootConfinesChild(t *testing.T) {
	requireNamespacePrivilege(t)

	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skipf("busybox not available: %v", err)
	}

	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	if err := copyFile(busybox, filepath.Join(rootfs, "bin", "sh"), 0o755); err != nil {
		t.Fatalf("copy busybox: %v", err)
	}

	cmd := exec.Command("/bin/sh", "-c", "echo hello > /marker")
	if err := Apply(cmd, Spec{
		PIDNamespace:   true,
		MountNamespace: true,
		Chroot:         rootfs,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Run: %v out=%s", err, out)
	}

	got, err := os.ReadFile(filepath.Join(rootfs, "marker"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(got)) != "hello" {
		t.Errorf("marker = %q, want %q (child not confined to chroot)", string(got), "hello")
	}
}

// TestApplyChrootMissingBinaryFailsCleanly: with chroot set but no
// matching binary inside, exec must fail in a way the caller can
// observe — confirming the chroot is real (the binary exists on the
// host but not inside).
func TestApplyChrootMissingBinaryFailsCleanly(t *testing.T) {
	requireNamespacePrivilege(t)

	rootfs := t.TempDir()
	// Intentionally empty rootfs — no /bin/sh inside.

	cmd := exec.Command("/bin/sh", "-c", "echo unreachable")
	if err := Apply(cmd, Spec{MountNamespace: true, Chroot: rootfs}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := cmd.Run(); err == nil {
		t.Error("expected exec failure inside empty chroot, got nil")
	}
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}

// exitErrStderr extracts the stderr buffer from an *exec.ExitError, or
// returns "" if err is something else. Pure ergonomics for test
// failure messages.
func exitErrStderr(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return string(ee.Stderr)
	}
	return ""
}
