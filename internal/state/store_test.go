package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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

// AddApp must deep-copy the inbound Config so a later mutation by
// the caller can't reach back into the persisted snapshot. Without
// supervisor.CloneConfig, mutating cfg.Env (or Args, or
// Sandbox.UIDMappings) after AddApp would silently change what gets
// written on the next flush.
func TestAddAppDeepCopiesInboundConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	envOriginal := []string{"FOO=bar"}
	argsOriginal := []string{"server.js"}
	cfg := supervisor.Config{
		ID:      "alias-test",
		Command: "bun",
		Args:    argsOriginal,
		Env:     envOriginal,
		Port:    9000,
		Sandbox: &sandbox.Spec{
			UserNamespace: true,
			UIDMappings:   []sandbox.IDMap{{ContainerID: 0, HostID: 100000, Size: 65536}},
		},
		CgroupLimits: &cgroup.Limits{MemoryMax: 64 * 1024 * 1024},
	}
	if err := s.AddApp(cfg); err != nil {
		t.Fatalf("AddApp: %v", err)
	}

	// Mutate every aliasable surface on the caller's copy AFTER the
	// store should have taken ownership.
	cfg.Args[0] = "tampered.js"
	cfg.Env[0] = "FOO=tampered"
	cfg.Env = append(cfg.Env, "INJECTED=yes")
	cfg.Sandbox.UIDMappings[0].HostID = 0 // attacker wants real root mapped in
	cfg.CgroupLimits.MemoryMax = 999

	// Read back via Apps(). The deep copy on insertion should mean
	// none of the caller's mutations leaked through.
	got := s.Apps()
	if len(got) != 1 {
		t.Fatalf("Apps() = %d entries, want 1", len(got))
	}
	stored := got[0]
	if stored.Args[0] != "server.js" {
		t.Errorf("Args[0] = %q; mutation leaked through (want server.js)", stored.Args[0])
	}
	if len(stored.Env) != 1 || stored.Env[0] != "FOO=bar" {
		t.Errorf("Env = %v; mutation leaked through (want [FOO=bar])", stored.Env)
	}
	if stored.Sandbox.UIDMappings[0].HostID != 100000 {
		t.Errorf("Sandbox.UIDMappings[0].HostID = %d; mutation leaked through (want 100000)",
			stored.Sandbox.UIDMappings[0].HostID)
	}
	if stored.CgroupLimits.MemoryMax != 64*1024*1024 {
		t.Errorf("CgroupLimits.MemoryMax = %d; mutation leaked through",
			stored.CgroupLimits.MemoryMax)
	}

	// Reload from disk and confirm the on-disk state was never
	// polluted either (the next flush would have written it if so).
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	persisted := s2.Apps()
	if persisted[0].Env[0] != "FOO=bar" {
		t.Errorf("on-disk Env = %v; want [FOO=bar]", persisted[0].Env)
	}
}

// Apps() must also deep-copy on the way out so callers can't mutate
// the persisted snapshot through the returned slice.
func TestAppsDeepCopiesReturnedConfigs(t *testing.T) {
	s := newStore(t)
	cfg := supervisor.Config{
		ID:      "snapshot-test",
		Command: "bun",
		Env:     []string{"FOO=bar"},
		Port:    9000,
	}
	if err := s.AddApp(cfg); err != nil {
		t.Fatalf("AddApp: %v", err)
	}

	first := s.Apps()
	// Caller mutates what we returned.
	first[0].Env[0] = "FOO=tampered"
	first[0].Env = append(first[0].Env, "INJECTED=yes")

	// A subsequent Apps() must not show the mutation.
	second := s.Apps()
	if second[0].Env[0] != "FOO=bar" {
		t.Errorf("Env[0] = %q; first-caller mutation leaked back (want FOO=bar)",
			second[0].Env[0])
	}
	if len(second[0].Env) != 1 {
		t.Errorf("Env length = %d; first-caller append leaked back (want 1)",
			len(second[0].Env))
	}
}

func TestVolumeAddAndRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	v := supervisor.Volume{
		ID: "vol-a", BackingPath: "tenant-a/data", ReadOnly: false, FSType: "ext4",
	}
	if err := s1.AddVolume(v); err != nil {
		t.Fatalf("AddVolume: %v", err)
	}

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.Volumes()
	if len(got) != 1 {
		t.Fatalf("reload: %d volumes, want 1", len(got))
	}
	if got[0].ID != "vol-a" || got[0].BackingPath != "tenant-a/data" || got[0].FSType != "ext4" {
		t.Errorf("reload mismatch: %+v", got[0])
	}
}

func TestVolumeRemove(t *testing.T) {
	s := newStore(t)
	_ = s.AddVolume(supervisor.Volume{ID: "vol-a", BackingPath: "a/data"})
	_ = s.AddVolume(supervisor.Volume{ID: "vol-b", BackingPath: "b/data"})

	if err := s.RemoveVolume("vol-a"); err != nil {
		t.Fatalf("RemoveVolume: %v", err)
	}
	got := s.Volumes()
	if len(got) != 1 || got[0].ID != "vol-b" {
		t.Errorf("after remove: %+v", got)
	}
}

func TestVolumeRemoveUnknownIsNoop(t *testing.T) {
	s := newStore(t)
	if err := s.RemoveVolume("ghost"); err != nil {
		t.Errorf("RemoveVolume of unknown: %v", err)
	}
}

func TestVolumeAndAppCoexistInSameStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s1, _ := NewStore(path)

	_ = s1.AddVolume(supervisor.Volume{ID: "vol-a", BackingPath: "a/data"})
	_ = s1.AddApp(supervisor.Config{
		ID: "myapp", Command: "sleep", Port: 9000,
	})

	// Reload from disk; both should survive.
	s2, _ := NewStore(path)
	if len(s2.Volumes()) != 1 || s2.Volumes()[0].ID != "vol-a" {
		t.Errorf("volumes lost on reload: %+v", s2.Volumes())
	}
	if len(s2.Apps()) != 1 || s2.Apps()[0].ID != "myapp" {
		t.Errorf("apps lost on reload: %+v", s2.Apps())
	}
}

func TestVolumeAddRejectsEmptyID(t *testing.T) {
	s := newStore(t)
	if err := s.AddVolume(supervisor.Volume{BackingPath: "a/data"}); err == nil {
		t.Error("expected error for empty volume id")
	}
}

func TestVolumeOrderingAlphabeticalOnReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s1, _ := NewStore(path)
	_ = s1.AddVolume(supervisor.Volume{ID: "z", BackingPath: "z/data"})
	_ = s1.AddVolume(supervisor.Volume{ID: "a", BackingPath: "a/data"})
	_ = s1.AddVolume(supervisor.Volume{ID: "m", BackingPath: "m/data"})

	s2, _ := NewStore(path)
	got := s2.Volumes()
	want := []string{"a", "m", "z"}
	for i, v := range got {
		if v.ID != want[i] {
			t.Errorf("position %d: got %q, want %q", i, v.ID, want[i])
		}
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

// --- AppMetadata tests (envelope v6 calibration) ----------------------

func TestAddAppGeneratesMetadata(t *testing.T) {
	s := newStore(t)
	before := time.Now().UTC().Add(-time.Second) // 1s window for clock skew
	if err := s.AddApp(supervisor.Config{ID: "foo", Command: "sleep", Args: []string{"1"}, Port: 8000}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	m, ok := s.Meta("foo")
	if !ok {
		t.Fatal("Meta(\"foo\") returned false; metadata not generated")
	}
	if m.UID == "" {
		t.Errorf("Meta.UID = empty; want UUIDv7")
	}
	if u, err := uuid.Parse(m.UID); err != nil {
		t.Errorf("Meta.UID = %q, not a parseable UUID: %v", m.UID, err)
	} else if u.Version() != 7 {
		t.Errorf("Meta.UID version = %d, want 7 (UUIDv7)", u.Version())
	}
	if m.Generation != 1 {
		t.Errorf("Meta.Generation = %d, want 1 on first add", m.Generation)
	}
	if m.ResourceVersion != 1 {
		t.Errorf("Meta.ResourceVersion = %d, want 1 on first add", m.ResourceVersion)
	}
	if m.CreationTimestamp.Before(before) || m.CreationTimestamp.After(after) {
		t.Errorf("Meta.CreationTimestamp = %v, want within [%v, %v]",
			m.CreationTimestamp, before, after)
	}
}

func TestAddAppOverwritePreservesUIDAndBumpsGeneration(t *testing.T) {
	s := newStore(t)
	if err := s.AddApp(supervisor.Config{ID: "foo", Command: "sleep", Args: []string{"1"}, Port: 8000}); err != nil {
		t.Fatalf("AddApp #1: %v", err)
	}
	m1, _ := s.Meta("foo")

	// Wait a bit to ensure timestamp comparison would catch any
	// accidental regeneration.
	time.Sleep(10 * time.Millisecond)

	if err := s.AddApp(supervisor.Config{ID: "foo", Command: "sleep", Args: []string{"2"}, Port: 8001}); err != nil {
		t.Fatalf("AddApp #2 (deploy): %v", err)
	}
	m2, _ := s.Meta("foo")

	if m2.UID != m1.UID {
		t.Errorf("UID changed on overwrite: %q → %q (want preserved)", m1.UID, m2.UID)
	}
	if !m2.CreationTimestamp.Equal(m1.CreationTimestamp) {
		t.Errorf("CreationTimestamp changed on overwrite: %v → %v (want preserved)",
			m1.CreationTimestamp, m2.CreationTimestamp)
	}
	if m2.Generation != m1.Generation+1 {
		t.Errorf("Generation = %d, want %d (bump by 1)", m2.Generation, m1.Generation+1)
	}
	if m2.ResourceVersion != m1.ResourceVersion+1 {
		t.Errorf("ResourceVersion = %d, want %d (bump by 1)", m2.ResourceVersion, m1.ResourceVersion+1)
	}
}

func TestRemoveAppDropsMetadata(t *testing.T) {
	s := newStore(t)
	if err := s.AddApp(supervisor.Config{ID: "ephemeral", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	if err := s.RemoveApp("ephemeral"); err != nil {
		t.Fatalf("RemoveApp: %v", err)
	}
	if _, ok := s.Meta("ephemeral"); ok {
		t.Error("Meta returned ok after RemoveApp; metadata should be dropped")
	}

	// Re-spawn with same name → fresh UID (delete-then-recreate
	// breaks the chain per v5 design).
	if err := s.AddApp(supervisor.Config{ID: "ephemeral", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatalf("AddApp re-spawn: %v", err)
	}
	m, ok := s.Meta("ephemeral")
	if !ok {
		t.Fatal("Meta returned false after re-spawn")
	}
	if m.Generation != 1 {
		t.Errorf("Generation after re-spawn = %d, want 1 (fresh metadata)", m.Generation)
	}
}

func TestMetadataPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s1.AddApp(supervisor.Config{ID: "persistent", Command: "sleep", Args: []string{"1"}, Port: 9100}); err != nil {
		t.Fatalf("AddApp: %v", err)
	}
	want, _ := s1.Meta("persistent")

	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (reload): %v", err)
	}
	got, ok := s2.Meta("persistent")
	if !ok {
		t.Fatal("Meta after reload returned false")
	}
	if got.UID != want.UID {
		t.Errorf("UID after reload = %q, want %q", got.UID, want.UID)
	}
	if got.Generation != want.Generation {
		t.Errorf("Generation after reload = %d, want %d", got.Generation, want.Generation)
	}
	if got.ResourceVersion != want.ResourceVersion {
		t.Errorf("ResourceVersion after reload = %d, want %d", got.ResourceVersion, want.ResourceVersion)
	}
	if !got.CreationTimestamp.Equal(want.CreationTimestamp) {
		t.Errorf("CreationTimestamp after reload = %v, want %v",
			got.CreationTimestamp, want.CreationTimestamp)
	}
}

func TestLoadV1MigratesForward(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Hand-craft a v1 file (no metadata block).
	v1 := `{
  "version": 1,
  "apps": [
    {"config": {"id": "legacy", "command": "sleep", "args": ["30"], "port": 9500, "env": ["KEY=val"]}}
  ]
}`
	if err := os.WriteFile(path, []byte(v1), 0o600); err != nil {
		t.Fatalf("write v1 file: %v", err)
	}

	before := time.Now().UTC().Add(-time.Second)
	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (load v1): %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	// App content should round-trip.
	apps := s.Apps()
	if len(apps) != 1 || apps[0].ID != "legacy" {
		t.Fatalf("Apps after v1 load = %+v, want one entry id=legacy", apps)
	}

	// Metadata should be freshly generated.
	m, ok := s.Meta("legacy")
	if !ok {
		t.Fatal("Meta(\"legacy\") returned false after v1 migration")
	}
	if u, err := uuid.Parse(m.UID); err != nil {
		t.Errorf("migrated UID %q is not a parseable UUID: %v", m.UID, err)
	} else if u.Version() != 7 {
		t.Errorf("migrated UID version = %d, want 7", u.Version())
	}
	if m.Generation != 1 || m.ResourceVersion != 1 {
		t.Errorf("migrated metadata generation/rv = %d/%d, want 1/1", m.Generation, m.ResourceVersion)
	}
	if m.CreationTimestamp.Before(before) || m.CreationTimestamp.After(after) {
		t.Errorf("migrated CreationTimestamp = %v, want within [%v, %v]",
			m.CreationTimestamp, before, after)
	}

	// File should have been rewritten as v2.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read state.json: %v", err)
	}
	// Decode the v2 shape directly rather than substring-matching on
	// "metadata" — the latter could pass spuriously if a Config field
	// (env value, command, etc.) happened to contain the literal.
	var migrated struct {
		Version int   `json:"version"`
		Apps    []App `json:"apps"`
	}
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatalf("decode migrated file: %v", err)
	}
	if migrated.Version != FormatVersion {
		t.Errorf("post-migration version = %d, want %d (current FormatVersion)", migrated.Version, FormatVersion)
	}
	if len(migrated.Apps) != 1 {
		t.Fatalf("post-migration apps len = %d, want 1", len(migrated.Apps))
	}
	if _, err := uuid.Parse(migrated.Apps[0].Metadata.UID); err != nil {
		t.Errorf("post-migration metadata.uid = %q, want a parseable UUID: %v", migrated.Apps[0].Metadata.UID, err)
	}
	if migrated.Apps[0].Metadata.ResourceVersion == 0 {
		t.Error("post-migration metadata.resource_version is zero — migration should have set it to 1")
	}
}

func TestLoadFutureVersionRefuses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{"version": 999, "apps": []}`), 0o600); err != nil {
		t.Fatalf("write future-version file: %v", err)
	}
	if _, err := NewStore(path); err == nil {
		t.Error("NewStore accepted version=999; want error")
	}
}
