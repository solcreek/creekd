//go:build linux

package supervisor

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/sandbox"
)

// TestSupervisorSpawnInsidePIDNamespace: an app spawned with a PID
// namespace sees itself as pid 1. End-to-end through Supervisor.Spawn
// rather than calling sandbox.Apply directly — proves the wiring in
// startLocked composes the namespace flag into the clone3 syscall.
func TestSupervisorSpawnInsidePIDNamespace(t *testing.T) {
	requirePrivilegedCgroupSup(t) // namespace ops need the same caps

	sup := newTestSupervisor()
	out := filepath.Join(t.TempDir(), "child.pid")

	_, err := sup.Spawn(Config{
		ID:      "ns-pid",
		Command: "/bin/sh",
		Args:    []string{"-c", fmt.Sprintf("echo $$ > %s; sleep 30", out)},
		Port:    19500,
		Sandbox: &sandbox.Spec{PIDNamespace: true},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("ns-pid") })

	if !eventuallyTrue(2*time.Second, func() bool {
		data, _ := os.ReadFile(out)
		return len(data) > 0
	}) {
		t.Fatalf("child never wrote %s", out)
	}

	data, _ := os.ReadFile(out)
	got := strings.TrimSpace(string(data))
	if got != "1" {
		t.Errorf("child's $$ = %q (host PID), want 1 (namespaced PID)", got)
	}
}

// TestSupervisorSandboxComposesWithCgroup: a single Spawn that sets
// BOTH namespaces and cgroup limits should produce a child confined
// in both ways. We verify by reading /proc/<host-pid>/cgroup and
// /proc/<host-pid>/ns/pid.
func TestSupervisorSandboxComposesWithCgroup(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	sup := newTestSupervisor()
	sup.CgroupParent = fmt.Sprintf("creekd-sbcg-%d.slice", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = os.Remove("/sys/fs/cgroup/" + sup.CgroupParent)
	})

	app, err := sup.Spawn(Config{
		ID:      "combo",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    19501,
		Sandbox: &sandbox.Spec{
			PIDNamespace: true,
			UTSNamespace: true,
		},
		CgroupLimits: &cgroup.Limits{
			MemoryMax: 32 * 1024 * 1024,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	pid := app.PID()
	if pid == 0 {
		t.Fatal("no PID")
	}

	// Cgroup applied: /proc/<pid>/cgroup ends with /<parent>/<id>.
	cgData, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cgroup: %v", pid, err)
	}
	if want := "/" + sup.CgroupParent + "/" + app.ID; !strings.Contains(string(cgData), want) {
		t.Errorf("cgroup view = %q, want substring %q", strings.TrimSpace(string(cgData)), want)
	}

	// PID ns applied: /proc/<pid>/ns/pid is a different inode from
	// the supervisor's own /proc/self/ns/pid.
	childNS, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		t.Fatalf("readlink child ns/pid: %v", err)
	}
	parentNS, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		t.Fatalf("readlink self ns/pid: %v", err)
	}
	if childNS == parentNS {
		t.Errorf("child ns/pid = %q, same as parent — PID namespace did not isolate", childNS)
	}
}

// TestSupervisorSandboxNoNewPrivs spawns a child with
// Sandbox.NoNewPrivs=true and reads /proc/<pid>/status to confirm
// the kernel marked PR_SET_NO_NEW_PRIVS active. The supervisor wraps
// the spawn with `setpriv --no-new-privs --` (from util-linux); the
// inner binary then exec's with the no-new-privs bit sticky for life.
func TestSupervisorSandboxNoNewPrivs(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	if _, err := exec.LookPath("setpriv"); err != nil {
		t.Skipf("setpriv not installed: %v", err)
	}

	sup := newTestSupervisor()
	_, err := sup.Spawn(Config{
		ID:      "nnp",
		Command: "/bin/sleep",
		Args:    []string{"30"},
		Port:    19700,
		Sandbox: &sandbox.Spec{NoNewPrivs: true},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("nnp") })

	// Poll the status file — setpriv → exec is async after Start.
	pid := sup.Get("nnp").PID()
	if pid == 0 {
		t.Fatal("no PID")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err == nil && containsNoNewPrivs1(string(data)) {
			return // success
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	t.Errorf("PR_SET_NO_NEW_PRIVS not set; /proc/%d/status:\n%s",
		pid, headLines(string(data), 20))
}

// containsNoNewPrivs1 returns true when /proc/<pid>/status contains
// "NoNewPrivs:\t1" — the exact wording the kernel emits.
func containsNoNewPrivs1(s string) bool {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "NoNewPrivs:") && strings.Contains(line, "1") {
			return true
		}
	}
	return false
}

// headLines returns the first n lines of s — keeps the test
// failure message readable when /proc/<pid>/status is dumped.
func headLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestSupervisorSandboxChrootEndToEnd: build a minimal rootfs with a
// busybox-static copy at /bin/sh (so argv[0]="/bin/sh" dispatches the
// shell applet) and spawn a child via Supervisor. The child writes
// "hello" to /marker. If chroot was applied, the host observes the
// marker at <rootfs>/marker. If chroot was NOT applied, the child
// would have written to the host's /marker — a different path — and
// <rootfs>/marker would stay empty.
func TestSupervisorSandboxChrootEndToEnd(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	busybox, err := exec.LookPath("busybox")
	if err != nil {
		t.Skipf("busybox not installed: %v", err)
	}

	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := copyBinaryFile(busybox, filepath.Join(rootfs, "bin", "sh")); err != nil {
		t.Fatalf("copy busybox: %v", err)
	}
	marker := filepath.Join(rootfs, "marker")

	sup := newTestSupervisor()
	_, err = sup.Spawn(Config{
		ID:      "chroot-app",
		Command: "/bin/sh",
		Args:    []string{"-c", "echo hello > /marker; sleep 30"},
		Port:    19502,
		Sandbox: &sandbox.Spec{
			PIDNamespace:   true,
			MountNamespace: true,
			Chroot:         rootfs,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("chroot-app") })

	if !eventuallyTrue(3*time.Second, func() bool {
		data, _ := os.ReadFile(marker)
		return strings.TrimSpace(string(data)) == "hello"
	}) {
		data, _ := os.ReadFile(marker)
		t.Errorf("child marker missing or wrong: %q", string(data))
	}
}

func copyBinaryFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
