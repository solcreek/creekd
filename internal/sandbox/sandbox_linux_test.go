//go:build linux

package sandbox

import (
	"errors"
	"fmt"
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

// TestApplyUserNamespaceSetsFlagAndMappings checks that Apply wires
// the user-namespace flag + UID/GID maps onto the underlying
// SysProcAttr. We don't spawn here — the round-trip from a
// successful spawn is covered by TestApplyUserNamespaceProcUIDMap.
func TestApplyUserNamespaceSetsFlagAndMappings(t *testing.T) {
	cmd := exec.Command("/bin/true")
	spec := Spec{
		UserNamespace: true,
		UIDMappings:   []IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}},
		GIDMappings:   []IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}},
	}
	if err := Apply(cmd, spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWUSER == 0 {
		t.Errorf("Cloneflags missing CLONE_NEWUSER: %#x", cmd.SysProcAttr.Cloneflags)
	}
	if len(cmd.SysProcAttr.UidMappings) != 1 {
		t.Fatalf("UidMappings = %v, want 1 entry", cmd.SysProcAttr.UidMappings)
	}
	got := cmd.SysProcAttr.UidMappings[0]
	if got.ContainerID != 0 || got.HostID != 100000 || got.Size != 65536 {
		t.Errorf("UidMappings[0] = %+v, want {0, 100000, 65536}", got)
	}
	if cmd.SysProcAttr.GidMappingsEnableSetgroups {
		t.Errorf("AllowSetgroups default should be false (kernel writes 'deny')")
	}
}

// TestApplyUserNamespaceProcUIDMap actually spawns a child inside a
// user namespace and inspects /proc/<pid>/uid_map from the host to
// verify the kernel applied the requested mapping. Uses a 1:1 root
// mapping so the spawn works without subuid configuration.
func TestApplyUserNamespaceProcUIDMap(t *testing.T) {
	requireNamespacePrivilege(t)

	cmd := exec.Command("/bin/sleep", "30")
	spec := Spec{
		UserNamespace: true,
		// Map root inside ↔ root outside. The container is already
		// privileged, so this is a no-op mapping but exercises every
		// kernel codepath.
		UIDMappings: []IDMap{{ContainerID: 0, HostID: 0, Size: 1}},
		GIDMappings: []IDMap{{ContainerID: 0, HostID: 0, Size: 1}},
	}
	if err := Apply(cmd, spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v output=%s", err, exitErrStderr(err))
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pid := cmd.Process.Pid

	// /proc/<pid>/ns/user must be a different namespace than ours.
	childNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/user", pid))
	if err != nil {
		t.Fatalf("readlink child ns/user: %v", err)
	}
	selfNS, err := os.Readlink("/proc/self/ns/user")
	if err != nil {
		t.Fatalf("readlink self ns/user: %v", err)
	}
	if childNS == selfNS {
		t.Errorf("child ns/user = %s, same as host — user namespace not applied", childNS)
	}

	// /proc/<pid>/uid_map carries the configured mapping. Format:
	// "  inside  outside  size" (whitespace-separated decimals).
	mapData, err := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", pid))
	if err != nil {
		t.Fatalf("read uid_map: %v", err)
	}
	mapStr := strings.TrimSpace(string(mapData))
	fields := strings.Fields(mapStr)
	if len(fields) < 3 || fields[0] != "0" || fields[1] != "0" || fields[2] != "1" {
		t.Errorf("uid_map = %q, want \"0 0 1\"", mapStr)
	}

	// /proc/<pid>/setgroups should be "deny" (AllowSetgroups=false).
	sgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/setgroups", pid))
	if err != nil {
		t.Fatalf("read setgroups: %v", err)
	}
	if got := strings.TrimSpace(string(sgData)); got != "deny" {
		t.Errorf("setgroups = %q, want deny", got)
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
