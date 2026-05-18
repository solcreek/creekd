//go:build linux

package cgroup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the OOM-trigger helper. When CGROUP_TEST_ALLOC_MB
// is set, the process allocates that many MiB of resident memory
// (touching one byte per page so it actually faults in) and sleeps,
// instead of running the test suite. Used by the cgroup memory-limit
// integration test below.
func TestMain(m *testing.M) {
	if v := os.Getenv("CGROUP_TEST_ALLOC_MB"); v != "" {
		mb, _ := strconv.Atoi(v)
		buf := make([]byte, mb*1024*1024)
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = 1
		}
		// Hold the allocation visible until either parent kills us or
		// the OOM killer arrives.
		time.Sleep(10 * time.Second)
		_ = buf // keep alive for the linter
		return
	}
	os.Exit(m.Run())
}

// requirePrivilegedCgroup skips the test unless we can actually write
// to cgroup.subtree_control at the v2 root. Hosts without v2 (or
// without delegation) get a clear skip rather than a noisy failure.
func requirePrivilegedCgroup(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 not available at /sys/fs/cgroup: %v", err)
	}
	probe := "/sys/fs/cgroup/cgroup.subtree_control"
	f, err := os.OpenFile(probe, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cgroup v2 root not writable (need privileged container): %v", err)
	}
	_ = f.Close()
}

// testManager returns a Manager under a per-test unique parent slice,
// auto-cleaning the slice directory on test exit. Caller must still
// remove any sub-cgroups it Creates.
func testManager(t *testing.T) *Manager {
	t.Helper()
	requirePrivilegedCgroup(t)
	parent := fmt.Sprintf("creekd-test-%d.slice", time.Now().UnixNano())
	m := NewManager(parent)
	t.Cleanup(func() {
		_ = os.Remove(filepath.Join(m.Root, parent))
	})
	return m
}

func readContents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.TrimSpace(string(data))
}

func TestEnsureParentCreatesSlice(t *testing.T) {
	m := testManager(t)
	if err := m.EnsureParent(); err != nil {
		t.Fatalf("EnsureParent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, m.Parent)); err != nil {
		t.Errorf("parent slice missing: %v", err)
	}
	// Idempotent: second call must not error.
	if err := m.EnsureParent(); err != nil {
		t.Errorf("second EnsureParent: %v", err)
	}
}

func TestEnsureParentEmptyRejected(t *testing.T) {
	m := &Manager{Root: "/sys/fs/cgroup", Parent: ""}
	if err := m.EnsureParent(); err == nil {
		t.Error("expected error for empty Parent")
	}
}

func TestCreateAppliesMemoryLimit(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("memcheck", Limits{MemoryMax: 16 * 1024 * 1024})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	got := readContents(t, filepath.Join(c.Path(), "memory.max"))
	if got != "16777216" {
		t.Errorf("memory.max = %q, want 16777216", got)
	}
}

func TestCreateAppliesPidsLimit(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("pidcheck", Limits{PidsMax: 32})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	got := readContents(t, filepath.Join(c.Path(), "pids.max"))
	if got != "32" {
		t.Errorf("pids.max = %q, want 32", got)
	}
}

func TestCreateAppliesCPULimit(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("cpucheck", Limits{CPUQuota: 50_000, CPUPeriod: 100_000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	got := readContents(t, filepath.Join(c.Path(), "cpu.max"))
	if got != "50000 100000" {
		t.Errorf("cpu.max = %q, want %q", got, "50000 100000")
	}
}

func TestCreateCPUQuotaUsesDefaultPeriod(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("cpudef", Limits{CPUQuota: 25_000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	got := readContents(t, filepath.Join(c.Path(), "cpu.max"))
	want := fmt.Sprintf("25000 %d", DefaultPeriod)
	if got != want {
		t.Errorf("cpu.max = %q, want %q", got, want)
	}
}

func TestCreateZeroLimitsLeavesMax(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("unlimited", Limits{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	if got := readContents(t, filepath.Join(c.Path(), "memory.max")); got != "max" {
		t.Errorf("memory.max = %q, want max", got)
	}
	if got := readContents(t, filepath.Join(c.Path(), "pids.max")); got != "max" {
		t.Errorf("pids.max = %q, want max", got)
	}
}

func TestCreateRejectsInvalidName(t *testing.T) {
	m := testManager(t)
	for _, name := range []string{"", "with/slash", "null\x00byte"} {
		if _, err := m.Create(name, Limits{}); err == nil {
			t.Errorf("Create(%q) expected error", name)
		}
	}
}

// TestCloneIntoCgroupConfinesChild spawns a child via CLONE_INTO_CGROUP
// (UseCgroupFD) and verifies it lands in the expected sub-cgroup by
// reading /proc/<pid>/cgroup before the child exits.
func TestCloneIntoCgroupConfinesChild(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("clone", Limits{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	fd, err := c.OpenFD()
	if err != nil {
		t.Fatalf("OpenFD: %v", err)
	}
	defer fd.Close()

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    int(fd.Fd()),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	pid := cmd.Process.Pid
	procPath := fmt.Sprintf("/proc/%d/cgroup", pid)
	// /proc/<pid>/cgroup for v2 reads as "0::<path>".
	data, err := os.ReadFile(procPath)
	if err != nil {
		t.Fatalf("read %s: %v", procPath, err)
	}
	got := strings.TrimSpace(string(data))
	wantSuffix := "/" + m.Parent + "/" + c.Name()
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("/proc/%d/cgroup = %q, want suffix %q", pid, got, wantSuffix)
	}

	// cgroup.procs in the sub-cgroup must list our child.
	procs := readContents(t, filepath.Join(c.Path(), "cgroup.procs"))
	if !containsPID(procs, pid) {
		t.Errorf("cgroup.procs = %q, missing pid %d", procs, pid)
	}
}

func containsPID(content string, pid int) bool {
	for _, ln := range strings.Split(content, "\n") {
		if strings.TrimSpace(ln) == fmt.Sprintf("%d", pid) {
			return true
		}
	}
	return false
}

// TestMemoryLimitTriggersOOMKill spawns a child capped at 8 MiB that
// resident-allocates 64 MiB (via TestMain in this package — see
// CGROUP_TEST_ALLOC_MB), observes the SIGKILL exit, and confirms the
// cgroup's memory.events records the kill. This is the M5.5
// acceptance criterion for memory enforcement.
func TestMemoryLimitTriggersOOMKill(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("oom", Limits{MemoryMax: 8 * 1024 * 1024})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	fd, err := c.OpenFD()
	if err != nil {
		t.Fatalf("OpenFD: %v", err)
	}
	defer fd.Close()

	// Self-spawn: run our own test binary with no tests matching, but
	// the alloc env triggers TestMain's allocation path. The child
	// touches every page so RSS is real, then sleeps.
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "CGROUP_TEST_ALLOC_MB=64")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.SysProcAttr = &syscall.SysProcAttr{
		UseCgroupFD: true,
		CgroupFD:    int(fd.Fd()),
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitErr := cmd.Wait()

	// The cgroup must record an OOM kill.
	st, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.OOMKill == 0 {
		t.Errorf("expected memory.events:oom_kill > 0; got %+v, waitErr=%v",
			st, waitErr)
	}

	// The child must have died (non-nil error from Wait → exit != 0
	// or signalled).
	if waitErr == nil {
		t.Errorf("expected non-zero exit from OOM-killed child; got nil")
	}
}

// TestRemoveBlocksWhileBusy: removing a cgroup that still has tasks
// inside should return an error rather than silently succeed.
func TestRemoveBlocksWhileBusy(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("busy", Limits{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fd, err := c.OpenFD()
	if err != nil {
		t.Fatalf("OpenFD: %v", err)
	}
	defer fd.Close()

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: int(fd.Fd())}
	if err := cmd.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err = c.Remove()
	if err == nil {
		// Clean up before failing.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		t.Fatal("Remove succeeded with active process; want error")
	}
	if !errors.Is(err, syscall.EBUSY) && !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v, want EBUSY-related", err)
	}

	// Kill child + cleanup.
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	// Now Remove should succeed.
	if err := c.Remove(); err != nil {
		t.Errorf("Remove after kill: %v", err)
	}
}

// TestStatsBeforeAnyEvent: a fresh cgroup with no OOM activity must
// report zero counters.
func TestStatsBeforeAnyEvent(t *testing.T) {
	m := testManager(t)
	c, err := m.Create("quiet", Limits{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Remove() })

	st, err := c.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.OOMKill != 0 || st.OOM != 0 {
		t.Errorf("expected zero counters, got %+v", st)
	}
}
