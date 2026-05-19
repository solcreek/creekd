package state

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/sandbox"
	"github.com/solcreek/creekd/internal/supervisor"
)

// newStore returns a Store rooted at a fresh temp dir.
func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestNewStoreMissingFileIsEmpty(t *testing.T) {
	s := newStore(t)
	if got := s.Apps(); len(got) != 0 {
		t.Errorf("Apps() = %d entries, want 0 for missing file", len(got))
	}
}

func TestAddAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	cfg := supervisor.Config{
		ID: "myapp", Command: "sleep", Args: []string{"30"}, Port: 9000,
		Env: []string{"FOO=bar"},
	}
	if err := s1.AddApp(cfg); err != nil {
		t.Fatalf("AddApp: %v", err)
	}

	// Fresh store reads back the same content.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	got := s2.Apps()
	if len(got) != 1 {
		t.Fatalf("reload: %d apps, want 1", len(got))
	}
	if got[0].ID != "myapp" || got[0].Port != 9000 {
		t.Errorf("reload mismatch: %+v", got[0])
	}
	if len(got[0].Env) != 1 || got[0].Env[0] != "FOO=bar" {
		t.Errorf("Env not preserved: %+v", got[0].Env)
	}
}

func TestAddReplaceExisting(t *testing.T) {
	s := newStore(t)
	_ = s.AddApp(supervisor.Config{ID: "x", Port: 1000, Command: "v1"})
	_ = s.AddApp(supervisor.Config{ID: "x", Port: 2000, Command: "v2"})
	got := s.Apps()
	if len(got) != 1 || got[0].Command != "v2" || got[0].Port != 2000 {
		t.Errorf("replace failed: %+v", got)
	}
}

func TestRemoveApp(t *testing.T) {
	s := newStore(t)
	_ = s.AddApp(supervisor.Config{ID: "a", Port: 1000})
	_ = s.AddApp(supervisor.Config{ID: "b", Port: 2000})
	if err := s.RemoveApp("a"); err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}
	got := s.Apps()
	if len(got) != 1 || got[0].ID != "b" {
		t.Errorf("after Remove: %+v, want only b", got)
	}
}

func TestRemoveUnknownIsNoop(t *testing.T) {
	s := newStore(t)
	if err := s.RemoveApp("ghost"); err != nil {
		t.Errorf("RemoveApp on unknown: %v", err)
	}
}

func TestAddRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	if err := s.AddApp(supervisor.Config{Port: 1000}); err == nil {
		t.Error("expected error for empty id")
	}
}

func TestAppsReturnsSortedSnapshot(t *testing.T) {
	s := newStore(t)
	for _, id := range []string{"c", "a", "b"} {
		_ = s.AddApp(supervisor.Config{ID: id, Port: 1000})
	}
	got := s.Apps()
	want := []string{"a", "b", "c"}
	for i, g := range got {
		if g.ID != want[i] {
			t.Errorf("Apps()[%d].ID = %q, want %q (sorted)", i, g.ID, want[i])
		}
	}
}

func TestConcurrentAddRemove(t *testing.T) {
	s := newStore(t)
	const workers = 10
	const iter = 50

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iter; i++ {
				id := mkID(w, i)
				if err := s.AddApp(supervisor.Config{
					ID: id, Port: 1000 + i,
				}); err != nil {
					t.Errorf("AddApp: %v", err)
				}
				if err := s.RemoveApp(id); err != nil {
					t.Errorf("RemoveApp: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	if got := s.Apps(); len(got) != 0 {
		t.Errorf("after concurrent add/remove cycles: %d apps left, want 0", len(got))
	}
}

func TestPersistsCgroupAndSandboxNested(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s1, _ := NewStore(path)

	_ = s1.AddApp(supervisor.Config{
		ID: "nested", Command: "sleep", Args: []string{"30"}, Port: 1234,
		CgroupLimits: &cgroup.Limits{
			MemoryMax: 32 * 1024 * 1024,
			PidsMax:   16,
			CPUQuota:  50_000,
		},
		Sandbox: &sandbox.Spec{
			PIDNamespace:  true,
			UserNamespace: true,
			UIDMappings:   []sandbox.IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}},
		},
		NetIsolation: true,
	})

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.Apps()
	if len(got) != 1 {
		t.Fatalf("reload: %d apps", len(got))
	}
	c := got[0]
	if c.CgroupLimits == nil || c.CgroupLimits.MemoryMax != 32*1024*1024 {
		t.Errorf("CgroupLimits not preserved: %+v", c.CgroupLimits)
	}
	if c.Sandbox == nil || !c.Sandbox.UserNamespace ||
		len(c.Sandbox.UIDMappings) != 1 ||
		c.Sandbox.UIDMappings[0].HostID != 100000 {
		t.Errorf("Sandbox not preserved: %+v", c.Sandbox)
	}
	if !c.NetIsolation {
		t.Errorf("NetIsolation not preserved")
	}
}

func mkID(worker, iter int) string {
	out := []byte("w--i--")
	out[1] = byte('0' + (worker / 10 % 10))
	out[2] = byte('0' + (worker % 10))
	out[4] = byte('0' + (iter / 10 % 10))
	out[5] = byte('0' + (iter % 10))
	return string(out)
}
