package state

import (
	"os"
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

// When flushMap fails (here forced by a read-only state directory),
// AddApp must leave the in-memory map untouched. Without the copy-
// on-write fix, the in-memory state would race ahead of the on-disk
// state — a later successful flush from a different call would then
// persist what AddApp reported as failed.
func TestAddAppFlushFailureLeavesMemoryClean(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX write-mode bits; cannot force flush failure")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	// First write succeeds — establishes a baseline.
	first := supervisor.Config{ID: "first", Port: 1000, Command: "v1"}
	if err := s.AddApp(first); err != nil {
		t.Fatalf("baseline AddApp: %v", err)
	}

	// Force the next flush to fail by making the directory read-only.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	second := supervisor.Config{ID: "second", Port: 2000, Command: "v2"}
	if err := s.AddApp(second); err == nil {
		t.Fatal("AddApp should fail when directory is read-only")
	}

	// In-memory must not contain "second" — flush failed, so the
	// state should match what's on disk (only "first").
	got := s.Apps()
	if len(got) != 1 || got[0].ID != "first" {
		t.Errorf("after failed AddApp, in-memory has %d apps (%+v); want only first",
			len(got), got)
	}

	// Make the dir writable again and do a successful AddApp of a
	// third entry. If the in-memory cache had been polluted with
	// "second", this successful flush would persist it.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod rw: %v", err)
	}
	third := supervisor.Config{ID: "third", Port: 3000, Command: "v3"}
	if err := s.AddApp(third); err != nil {
		t.Fatalf("recover AddApp: %v", err)
	}

	// Reload from disk and verify only first + third exist; second
	// was never persisted.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	ids := make([]string, 0)
	for _, a := range s2.Apps() {
		ids = append(ids, a.ID)
	}
	if len(ids) != 2 || ids[0] != "first" || ids[1] != "third" {
		t.Errorf("after recover, disk has %v; want [first third]", ids)
	}
}

// Same property for RemoveApp: failed flush must not pop the entry
// from the in-memory cache.
func TestRemoveAppFlushFailureLeavesMemoryClean(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX write-mode bits; cannot force flush failure")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.AddApp(supervisor.Config{ID: "keepme", Port: 1000, Command: "v1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := s.RemoveApp("keepme"); err == nil {
		t.Fatal("RemoveApp should fail when directory is read-only")
	}

	got := s.Apps()
	if len(got) != 1 || got[0].ID != "keepme" {
		t.Errorf("after failed RemoveApp, in-memory = %+v; want [keepme]", got)
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
