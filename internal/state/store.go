package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/solcreek/creekd/internal/supervisor"
)

// FormatVersion is the schema version embedded in every written
// state file. Bump when the on-disk shape changes; readers refuse
// versions they don't recognise.
const FormatVersion = 1

// State is the on-disk shape of the persisted app set + volumes.
// Volumes are restored BEFORE Apps on daemon start so VolumeMount
// references resolve.
type State struct {
	Version int      `json:"version"`
	Volumes []Volume `json:"volumes,omitempty"`
	Apps    []App    `json:"apps"`
}

// App is one persisted entry. The supervisor.Config is stored
// verbatim — on restore the same Spawn call reconstructs the same
// runtime state (modulo ephemeral fields like PID).
type App struct {
	Config supervisor.Config `json:"config"`
}

// Volume is one persisted volume registration. The supervisor.Volume
// is stored verbatim — on restore the same RegisterVolume call
// reconstructs the in-memory registry + re-applies MS_PRIVATE.
type Volume struct {
	Volume supervisor.Volume `json:"volume"`
}

// Store serialises persisted state under a JSON file. Construct
// with NewStore; every mutation flushes synchronously to disk.
type Store struct {
	path string

	mu      sync.Mutex
	apps    map[string]supervisor.Config // appID → config
	volumes map[string]supervisor.Volume // volumeID → volume
}

// NewStore opens (or creates) the state file at path. If the file
// exists it is loaded into the in-memory cache; missing files are
// treated as an empty state. The parent directory is created on
// first save. Returns an error only on permission / unreadable-file
// failure — an empty file or absent file is normal.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:    path,
		apps:    make(map[string]supervisor.Config),
		volumes: make(map[string]supervisor.Volume),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the configured file path.
func (s *Store) Path() string { return s.path }

// Apps returns a snapshot of every persisted config in alphabetical
// order. Useful for replaying through supervisor.Spawn at startup.
//
// Each returned Config is a deep copy via supervisor.CloneConfig —
// caller mutations to Args, Env, CgroupLimits, or Sandbox can't leak
// back into the Store's persisted snapshot.
func (s *Store) Apps() []supervisor.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.apps))
	for id := range s.apps {
		ids = append(ids, id)
	}
	sortStrings(ids)
	out := make([]supervisor.Config, 0, len(ids))
	for _, id := range ids {
		out = append(out, supervisor.CloneConfig(s.apps[id]))
	}
	return out
}

// AddApp persists cfg. If an entry with the same ID already exists
// it is replaced — admin Spawn rejects duplicates upstream, so this
// behaviour matters only for the Deploy path which legitimately
// overwrites.
//
// Copy-on-write: builds a candidate map, flushes that to disk, and
// only on flush success does it swap s.apps. If the flush fails the
// in-memory cache stays at the pre-call state, matching the on-disk
// state. Without this, a failed flush would leave in-memory ahead of
// disk; a subsequent successful flush would then persist what was
// reported as failed.
func (s *Store) AddApp(cfg supervisor.Config) error {
	if cfg.ID == "" {
		return errors.New("state: empty app id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneMap(s.apps)
	// Deep-clone the inbound config so a later mutation by the caller
	// (e.g. appending to cfg.Env) cannot reach back into our snapshot.
	next[cfg.ID] = supervisor.CloneConfig(cfg)
	if err := s.flushMap(next); err != nil {
		return err
	}
	s.apps = next
	return nil
}

// RemoveApp drops the entry for id. Removing an unknown id is a
// no-op (the on-disk state is already in sync). Same copy-on-write
// rule as AddApp — a flush failure leaves the in-memory cache intact.
func (s *Store) RemoveApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return nil
	}
	next := cloneMap(s.apps)
	delete(next, id)
	if err := s.flushMap(next); err != nil {
		return err
	}
	s.apps = next
	return nil
}

// load reads the JSON file into the in-memory cache. Missing file
// → empty cache, no error. Corrupted JSON or unknown version → error.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("state: read %s: %w", s.path, err)
	}
	if len(data) == 0 {
		return nil
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state: decode %s: %w", s.path, err)
	}
	if st.Version != FormatVersion {
		return fmt.Errorf("state: unsupported version %d (want %d)",
			st.Version, FormatVersion)
	}
	for _, v := range st.Volumes {
		if v.Volume.ID == "" {
			continue
		}
		s.volumes[v.Volume.ID] = v.Volume
	}
	for _, a := range st.Apps {
		if a.Config.ID == "" {
			continue
		}
		s.apps[a.Config.ID] = a.Config
	}
	return nil
}

// Volumes returns a snapshot of every persisted Volume in
// alphabetical order. Replayed at daemon start before any Apps so
// Spawn's VolumeMount references resolve.
func (s *Store) Volumes() []supervisor.Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.volumes))
	for id := range s.volumes {
		ids = append(ids, id)
	}
	sortStrings(ids)
	out := make([]supervisor.Volume, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.volumes[id])
	}
	return out
}

// AddVolume persists v. Re-adding with the same ID overwrites,
// matching the supervisor's idempotent re-register semantics.
func (s *Store) AddVolume(v supervisor.Volume) error {
	if v.ID == "" {
		return errors.New("state: empty volume id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nextVolumes := cloneVolumeMap(s.volumes)
	nextVolumes[v.ID] = v
	if err := s.flushBoth(s.apps, nextVolumes); err != nil {
		return err
	}
	s.volumes = nextVolumes
	return nil
}

// RemoveVolume drops the entry for id. Unknown id is a no-op.
func (s *Store) RemoveVolume(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.volumes[id]; !ok {
		return nil
	}
	nextVolumes := cloneVolumeMap(s.volumes)
	delete(nextVolumes, id)
	if err := s.flushBoth(s.apps, nextVolumes); err != nil {
		return err
	}
	s.volumes = nextVolumes
	return nil
}

// cloneVolumeMap is the map-level counterpart to cloneMap, used for
// the same copy-on-write swap.
func cloneVolumeMap(src map[string]supervisor.Volume) map[string]supervisor.Volume {
	dst := make(map[string]supervisor.Volume, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// flushMap writes the given app map snapshot to disk atomically.
// Used by AddApp / RemoveApp; preserves the current volumes map.
// Caller must hold s.mu.
func (s *Store) flushMap(m map[string]supervisor.Config) error {
	return s.flushBoth(m, s.volumes)
}

// flushBoth writes both maps atomically. Used by Volume mutations.
// Caller must hold s.mu.
func (s *Store) flushBoth(apps map[string]supervisor.Config, volumes map[string]supervisor.Volume) error {
	// Mode 0o700 / 0o600 — state.json contains every supervised
	// app's full Config (env vars, commands, volume refs). World-
	// readable defaults would leak tenant secrets to any local
	// process. Pentest H6 surfaced the previous 0o755/0o644.
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", filepath.Dir(s.path), err)
	}
	st := State{Version: FormatVersion}

	volIDs := make([]string, 0, len(volumes))
	for id := range volumes {
		volIDs = append(volIDs, id)
	}
	sortStrings(volIDs)
	for _, id := range volIDs {
		st.Volumes = append(st.Volumes, Volume{Volume: volumes[id]})
	}

	appIDs := make([]string, 0, len(apps))
	for id := range apps {
		appIDs = append(appIDs, id)
	}
	sortStrings(appIDs)
	for _, id := range appIDs {
		st.Apps = append(st.Apps, App{Config: apps[id]})
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("state: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename %s → %s: %w", tmp, s.path, err)
	}
	return nil
}

// cloneMap returns a copy of src at the map level only — Config
// values still share the same Args/Env slices and CgroupLimits/
// Sandbox pointer targets with src. That's fine for the copy-on-
// write swap (we throw next away on flush failure), because the
// inbound Config from AddApp goes through supervisor.CloneConfig
// before landing in next, and Apps() clones again on the way out.
// In other words: aliasing inside the Store is permitted because no
// internal code mutates a stored Config; aliasing across the Store
// boundary is what supervisor.CloneConfig prevents.
func cloneMap(src map[string]supervisor.Config) map[string]supervisor.Config {
	dst := make(map[string]supervisor.Config, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// sortStrings sorts in-place. Inlined to avoid an explicit "sort"
// import for the one-call use case.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
