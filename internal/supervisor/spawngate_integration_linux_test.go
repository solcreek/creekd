//go:build linux

package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
)

func cgroupSup(t *testing.T) *Supervisor {
	t.Helper()
	requirePrivilegedCgroupSup(t)
	sup := newTestSupervisor()
	sup.CgroupParent = fmt.Sprintf("creekd-gate-%d.slice", time.Now().UnixNano())
	t.Cleanup(func() { _ = os.Remove("/sys/fs/cgroup/" + sup.CgroupParent) })
	return sup
}

// Children an app forks the moment it starts are in its cgroup: the
// gate was moved there before the app's first instruction.
func TestSpawnGateChildrenStartInTheCgroup(t *testing.T) {
	sup := cgroupSup(t)
	app, err := sup.Spawn(Config{
		ID:           "gate-fork",
		Command:      "/bin/sh",
		Args:         []string{"-c", "for i in 1 2 3 4 5 6 7 8 9 10; do sleep 30 & done; wait"},
		Port:         19520,
		CgroupLimits: &cgroup.Limits{PidsMax: 64},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("gate-fork") })

	procs := filepath.Join(app.Cgroup().Path(), "cgroup.procs")
	if !eventuallyTrue(3*time.Second, func() bool {
		data, _ := os.ReadFile(procs)
		return len(strings.Fields(string(data))) >= 11 // sh + 10 sleeps
	}) {
		data, _ := os.ReadFile(procs)
		t.Fatalf("cgroup.procs = %q, want sh and its 10 children", data)
	}
	data, _ := os.ReadFile(procs)
	if !strings.Contains(string(data), fmt.Sprint(app.PID())) {
		t.Errorf("the app's PID %d (the gate, exec'd) is not in its cgroup: %q", app.PID(), data)
	}
}

// When the gate cannot be placed in the cgroup, the spawn fails and the
// app never runs.
func TestSpawnGateAbortsWhenCgroupJoinFails(t *testing.T) {
	sup := cgroupSup(t)
	orig := joinCgroup
	joinCgroup = func(*cgroup.Cgroup, int) error { return errors.New("injected: cgroup.procs not writable") }
	t.Cleanup(func() { joinCgroup = orig })

	marker := filepath.Join(t.TempDir(), "ran")
	_, err := sup.Spawn(Config{
		ID:           "gate-abort",
		Command:      "/bin/sh",
		Args:         []string{"-c", "touch " + marker + "; sleep 30"},
		Port:         19521,
		CgroupLimits: &cgroup.Limits{PidsMax: 64},
	})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("Spawn err = %v, want the join failure", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the app ran although its spawn was aborted: %v", err)
	}
	if sup.Get("gate-abort") != nil {
		t.Error("an aborted spawn left the app registered")
	}
}
