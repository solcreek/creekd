package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/solcreek/creekd/internal/supervisor"
)

// FormatVersion is the schema version embedded in every written
// state file. Bump when the on-disk shape changes; readers refuse
// versions they don't recognise.
//
// v1 → v2 (2026-05-24): adds AppMetadata (uid / generation /
// resourceVersion / creationTimestamp) per app, persisted alongside
// Config. v1 files are migrated forward on load — each app gets a
// freshly-generated UUIDv7 and creationTimestamp = time.Now() at
// migration time (best-effort; the true original creation moment is
// lost). Migration writes v2 back to disk transparently.
const FormatVersion = 2

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
// runtime state (modulo ephemeral fields like PID). AppMetadata is
// the envelope-layer persistent identity (uid + generation + rv +
// creationTimestamp) per DESIGN-self-host-state.md §"The interop-
// bearing subset".
type App struct {
	Config   supervisor.Config `json:"config"`
	Metadata AppMetadata       `json:"metadata"`
}

// AppMetadata is the per-app envelope-layer persistent identity.
// Generated on first AddApp; preserved across Deploy (which bumps
// Generation + ResourceVersion but keeps UID + CreationTimestamp);
// preserved across restore (uid + creationTimestamp NEVER
// regenerated).
type AppMetadata struct {
	// UID is the UUIDv7 stable identity. Never reused even across
	// delete+recreate with the same name.
	UID string `json:"uid"`
	// Generation bumps on every successful spec write
	// (AddApp on an existing ID). Does NOT bump on status writes
	// or annotation/label changes.
	Generation int64 `json:"generation"`
	// ResourceVersion bumps on every write (spec or status). Served
	// to clients as a string per K8s wire convention. Clients MUST
	// NOT do arithmetic on it.
	ResourceVersion uint64 `json:"resource_version"`
	// CreationTimestamp is RFC3339 at first AddApp; immutable;
	// preserved across restore.
	CreationTimestamp time.Time `json:"creation_timestamp"`
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

	mu       sync.Mutex
	apps     map[string]supervisor.Config // appID → config
	metadata map[string]AppMetadata       // appID → envelope metadata
	volumes  map[string]supervisor.Volume // volumeID → volume
}

// newAppMetadata generates fresh envelope metadata for a brand-new
// app. UUIDv7 (RFC 9562). Generation starts at 1 (the create write
// counts); ResourceVersion starts at 1 likewise. CreationTimestamp
// is the moment of creation in UTC.
func newAppMetadata() AppMetadata {
	u, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 only returns an error if the system can't
		// produce 6 bytes of randomness — extraordinary. Fall back
		// to v4 so the caller never panics on AddApp.
		u = uuid.New()
	}
	return AppMetadata{
		UID:               u.String(),
		Generation:        1,
		ResourceVersion:   1,
		CreationTimestamp: time.Now().UTC(),
	}
}

// NewStore opens (or creates) the state file at path. If the file
// exists it is loaded into the in-memory cache; missing files are
// treated as an empty state. The parent directory is created on
// first save. Returns an error only on permission / unreadable-file
// failure — an empty file or absent file is normal.
//
// v1 → v2 migration: if the loaded file is FormatVersion=1, each
// app gets freshly-generated metadata (UUIDv7, generation=1, rv=1,
// creationTimestamp=time.Now()) and the file is rewritten as v2.
// The migration is transparent — callers see only the v2 shape.
func NewStore(path string) (*Store, error) {
	s := &Store{
		path:     path,
		apps:     make(map[string]supervisor.Config),
		metadata: make(map[string]AppMetadata),
		volumes:  make(map[string]supervisor.Volume),
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
// Metadata semantics:
//   - First AddApp for an ID: generates fresh AppMetadata (UUIDv7 +
//     generation=1 + rv=1 + creationTimestamp=now).
//   - Overwriting AddApp (Deploy): preserves UID + CreationTimestamp;
//     bumps Generation by 1 and ResourceVersion by 1.
//
// Copy-on-write: builds candidate maps, flushes them to disk, and
// only on flush success swaps s.apps + s.metadata. If the flush fails
// the in-memory cache stays at the pre-call state, matching the
// on-disk state.
func (s *Store) AddApp(cfg supervisor.Config) error {
	if cfg.ID == "" {
		return errors.New("state: empty app id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	nextApps := cloneMap(s.apps)
	// Deep-clone the inbound config so a later mutation by the caller
	// (e.g. appending to cfg.Env) cannot reach back into our snapshot.
	nextApps[cfg.ID] = supervisor.CloneConfig(cfg)
	nextMeta := cloneMetadataMap(s.metadata)
	if existing, ok := s.metadata[cfg.ID]; ok {
		// Deploy / overwrite: preserve identity, bump versions.
		nextMeta[cfg.ID] = AppMetadata{
			UID:               existing.UID,
			CreationTimestamp: existing.CreationTimestamp,
			Generation:        existing.Generation + 1,
			ResourceVersion:   existing.ResourceVersion + 1,
		}
	} else {
		nextMeta[cfg.ID] = newAppMetadata()
	}
	if err := s.flushFull(nextApps, nextMeta, s.volumes); err != nil {
		return err
	}
	s.apps = nextApps
	s.metadata = nextMeta
	return nil
}

// RemoveApp drops the entry for id. Removing an unknown id is a
// no-op (the on-disk state is already in sync). Same copy-on-write
// rule as AddApp — a flush failure leaves the in-memory cache intact.
//
// Per v5 design (delete-then-recreate with same name gets fresh uid),
// removal drops the metadata too — there is no carry-over of the
// previous UID if the same name is later re-spawned.
func (s *Store) RemoveApp(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.apps[id]; !ok {
		return nil
	}
	nextApps := cloneMap(s.apps)
	delete(nextApps, id)
	nextMeta := cloneMetadataMap(s.metadata)
	delete(nextMeta, id)
	if err := s.flushFull(nextApps, nextMeta, s.volumes); err != nil {
		return err
	}
	s.apps = nextApps
	s.metadata = nextMeta
	return nil
}

// Meta returns the persisted envelope metadata for id. The bool is
// false when no such app exists.
func (s *Store) Meta(id string) (AppMetadata, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metadata[id]
	return m, ok
}

// load reads the JSON file into the in-memory cache. Missing file
// → empty cache, no error. Corrupted JSON or unknown version → error.
//
// v1 files are migrated forward to v2 transparently: each loaded app
// gets fresh metadata (UUIDv7 + creationTimestamp=time.Now()) and the
// migrated state is written back. Older formats (v0 / future v3+)
// are rejected.
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
	// Peek at the version field before full-shape decode — v1 has a
	// different App shape (no metadata block) so a single struct
	// decode would silently zero the new fields for v1 files.
	var head struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return fmt.Errorf("state: decode version header %s: %w", s.path, err)
	}
	switch head.Version {
	case 1:
		if err := s.loadV1(data); err != nil {
			return err
		}
		// Persist forward as v2 so subsequent boots take the v2 path.
		// flushBoth requires s.mu held. load() runs from the
		// constructor so there's currently no contention, but taking
		// the lock keeps the documented contract honest in case
		// load() is ever called from a reload context later.
		s.mu.Lock()
		err := s.flushBoth(s.apps, s.volumes)
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("state: v1→v2 migration flush %s: %w", s.path, err)
		}
		return nil
	case FormatVersion:
		return s.loadV2(data)
	default:
		return fmt.Errorf("state: unsupported version %d in %s (want %d)",
			head.Version, s.path, FormatVersion)
	}
}

// loadV1 decodes the legacy v1 shape ({"version":1,"apps":[{"config":...}]})
// and generates fresh AppMetadata for each app.
func (s *Store) loadV1(data []byte) error {
	type appV1 struct {
		Config supervisor.Config `json:"config"`
	}
	var st struct {
		Version int      `json:"version"`
		Volumes []Volume `json:"volumes,omitempty"`
		Apps    []appV1  `json:"apps"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state: decode v1 %s: %w", s.path, err)
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
		s.metadata[a.Config.ID] = newAppMetadata()
	}
	return nil
}

// loadV2 decodes the current State shape.
func (s *Store) loadV2(data []byte) error {
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state: decode v2 %s: %w", s.path, err)
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
		// Defensive: if a v2 file is missing metadata (shouldn't
		// happen with our writer, but a hand-edited file might),
		// fall back to a freshly-generated stamp.
		if a.Metadata.UID == "" {
			s.metadata[a.Config.ID] = newAppMetadata()
		} else {
			s.metadata[a.Config.ID] = a.Metadata
		}
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

// flushBoth writes apps + the current metadata snapshot + the given
// volumes map atomically. Used by Volume mutations + as the v1→v2
// migration writeback path. Caller must hold s.mu.
func (s *Store) flushBoth(apps map[string]supervisor.Config, volumes map[string]supervisor.Volume) error {
	return s.flushFull(apps, s.metadata, volumes)
}

// flushFull writes all three maps atomically. Used by AddApp /
// RemoveApp when metadata is also changing. Caller must hold s.mu.
func (s *Store) flushFull(apps map[string]supervisor.Config, metadata map[string]AppMetadata, volumes map[string]supervisor.Volume) error {
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
		st.Apps = append(st.Apps, App{
			Config:   apps[id],
			Metadata: metadata[id],
		})
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

// cloneMetadataMap is the AppMetadata-typed counterpart to cloneMap.
// AppMetadata is a small value struct (~64 bytes); pure value copy.
func cloneMetadataMap(src map[string]AppMetadata) map[string]AppMetadata {
	dst := make(map[string]AppMetadata, len(src)+1)
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
