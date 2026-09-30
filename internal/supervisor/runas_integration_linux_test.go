//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSupervisorRunAsIsolatesEnviron: two apps under distinct RunAs
// UIDs cannot read each other's /proc/<pid>/environ, where secrets
// live; the same probe running as root can. That control is the point:
// it proves the probe detects the leak this change closes.
func TestSupervisorRunAsIsolatesEnviron(t *testing.T) {
	requirePrivilegedCgroupSup(t)

	sup := newTestSupervisor()
	// A world-writable scratch dir: the probes run as non-root UIDs and
	// t.TempDir() is 0700 root.
	dir, err := os.MkdirTemp("", "creekd-runas-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}

	victim, err := sup.Spawn(Config{
		ID:      "victim",
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 30"},
		Env:     []string{"CREEKD_TEST_SECRET=s3cret-value"},
		Port:    19510,
		RunAs:   &RunAs{UID: 61001, GID: 61001},
	})
	if err != nil {
		t.Fatalf("Spawn victim: %v", err)
	}
	t.Cleanup(func() { _ = sup.Stop("victim") })
	if victim.PID() == 0 {
		t.Fatal("victim has no PID")
	}

	// probe reads the victim's environ and records who it ran as.
	probe := func(id string, port int, runAs *RunAs, root bool) (environ, uid, capEff string) {
		t.Helper()
		out := filepath.Join(dir, id)
		script := fmt.Sprintf(
			"cat /proc/%d/environ > %s.env 2>/dev/null; id -u > %s.uid; grep CapEff /proc/self/status > %s.cap; sleep 30",
			victim.PID(), out, out, out)
		if _, err := sup.Spawn(Config{
			ID: id, Command: "/bin/sh", Args: []string{"-c", script}, Port: port,
			RunAs: runAs, RunAsRoot: root,
		}); err != nil {
			t.Fatalf("Spawn %s: %v", id, err)
		}
		t.Cleanup(func() { _ = sup.Stop(id) })
		if !eventuallyTrue(3*time.Second, func() bool {
			data, _ := os.ReadFile(out + ".cap")
			return len(data) > 0
		}) {
			t.Fatalf("%s never finished probing", id)
		}
		read := func(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
		return read(out + ".env"), read(out + ".uid"), read(out + ".cap")
	}

	env, uid, capEff := probe("tenant", 19511, &RunAs{UID: 61002, GID: 61002}, false)
	if strings.Contains(env, "s3cret-value") {
		t.Errorf("a tenant under its own UID read another tenant's environ: %q", env)
	}
	if uid != "61002" {
		t.Errorf("tenant ran as uid %q, want 61002", uid)
	}
	if !strings.HasSuffix(capEff, "0000000000000000") {
		t.Errorf("tenant kept capabilities: %q", capEff)
	}

	env, _, _ = probe("rootprobe", 19512, nil, true)
	if !strings.Contains(env, "CREEKD_TEST_SECRET=s3cret-value") {
		t.Errorf("control: root should read the victim's environ, got %q — the probe is not detecting the leak", env)
	}
}
