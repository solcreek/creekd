//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
)

// requirePrivilegedCgroupSup mirrors the cgroup package's gate: tests
// only run when /sys/fs/cgroup is writable (i.e. inside a privileged
// container or on bare-metal as root). Unprivileged Docker hits the
// skip path with a clear message.
func requirePrivilegedCgroupSup(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		t.Skipf("cgroup v2 not available: %v", err)
	}
	probe := "/sys/fs/cgroup/cgroup.subtree_control"
	f, err := os.OpenFile(probe, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cgroup v2 root not writable: %v", err)
	}
	_ = f.Close()
}

// TestSupervisorSpawnsChildIntoCgroup is the M5.5 supervisor-level
// acceptance: spawn an app with CgroupLimits set, verify the child's
// /proc/<pid>/cgroup ends with the per-app slice, and verify
// memory.max is what we asked for.
func TestSupervisorSpawnsChildIntoCgroup(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	sup := newTestSupervisor()
	sup.CgroupParent = fmt.Sprintf("creekd-sup-test-%d.slice", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = os.Remove("/sys/fs/cgroup/" + sup.CgroupParent)
	})

	app, err := sup.Spawn(Config{
		ID:      "limited",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    19000,
		CgroupLimits: &cgroup.Limits{
			MemoryMax: 32 * 1024 * 1024, // 32 MiB
			PidsMax:   16,
			CPUQuota:  50_000,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop(app.ID) })

	pid := app.PID()
	if pid == 0 {
		t.Fatal("expected non-zero PID")
	}

	// /proc/<pid>/cgroup for v2 lists the cgroup path. We expect it to
	// end with /<parent>/<appID>.
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		t.Fatalf("read /proc/%d/cgroup: %v", pid, err)
	}
	got := strings.TrimSpace(string(data))
	wantSuffix := "/" + sup.CgroupParent + "/" + app.ID
	if !strings.HasSuffix(got, wantSuffix) {
		t.Errorf("cgroup line = %q, want suffix %q", got, wantSuffix)
	}

	// memory.max applied.
	memMaxPath := filepath.Join("/sys/fs/cgroup", sup.CgroupParent, app.ID, "memory.max")
	data, err = os.ReadFile(memMaxPath)
	if err != nil {
		t.Fatalf("read memory.max: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "33554432" {
		t.Errorf("memory.max = %q, want 33554432", got)
	}
}

// TestSupervisorRejectsCgroupLimitsWithoutParent: CgroupLimits set but
// Supervisor.CgroupParent empty must surface a clear error from Spawn.
func TestSupervisorRejectsCgroupLimitsWithoutParent(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	sup := newTestSupervisor()
	// CgroupParent intentionally left empty.

	_, err := sup.Spawn(Config{
		ID:           "no-parent",
		Command:      "sleep",
		Args:         []string{"30"},
		Port:         19001,
		CgroupLimits: &cgroup.Limits{MemoryMax: 16 * 1024 * 1024},
	})
	if err == nil {
		t.Fatal("expected error when CgroupLimits set but CgroupParent empty")
	}
	if !strings.Contains(err.Error(), "CgroupParent") {
		t.Errorf("err = %v, want mention of CgroupParent", err)
	}
}

// TestSupervisorCgroupRemovedAfterStop verifies the supervisor cleans
// up the per-app cgroup directory once Stop completes. Leaks here
// would accumulate over many deploys.
func TestSupervisorCgroupRemovedAfterStop(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	sup := newTestSupervisor()
	sup.CgroupParent = fmt.Sprintf("creekd-cleanup-%d.slice", time.Now().UnixNano())
	t.Cleanup(func() {
		_ = os.Remove("/sys/fs/cgroup/" + sup.CgroupParent)
	})

	app, err := sup.Spawn(Config{
		ID:      "ephemeral",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    19002,
		CgroupLimits: &cgroup.Limits{
			MemoryMax: 16 * 1024 * 1024,
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	cgPath := filepath.Join("/sys/fs/cgroup", sup.CgroupParent, app.ID)
	if _, err := os.Stat(cgPath); err != nil {
		t.Fatalf("cgroup dir missing pre-Stop: %v", err)
	}

	if err := sup.Stop(app.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(cgPath); err == nil {
		t.Errorf("cgroup dir %s still present after Stop", cgPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("unexpected stat err: %v", err)
	}
}
