package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/solcreek/creekd/internal/supervisor"
)

// UnsupportedFilesystemError is returned by NewStore when the data
// directory lives on a filesystem that doesn't meet the rename(2) +
// fsync(dir) durability contract creekd requires. See
// fscheck_linux.go for the magic constants checked.
type UnsupportedFilesystemError struct {
	Path       string
	Detected   string
	MagicValue int64
}

func (e *UnsupportedFilesystemError) Error() string {
	return fmt.Sprintf("state: unsupported filesystem at %s (%s); creekd requires ext4 or xfs",
		e.Path, e.Detected)
}

// StorageCorruptedError is returned by flushFull when read-back
// verification fails — typically a silent fsync EIO that the
// kernel can do on ext4 ("fsyncgate"). Surfaces FATAL so the daemon
// refuses to keep writing into a corrupted store.
type StorageCorruptedError struct {
	Path       string
	WantSHA256 string
	GotSHA256  string
}

func (e *StorageCorruptedError) Error() string {
	return fmt.Sprintf("state: read-back verify failed for %s (wrote sha256:%s, re-read sha256:%s); "+
		"fsync may have silently failed — refusing further writes",
		e.Path, e.WantSHA256, e.GotSHA256)
}

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
//
// v2 → v3 (2026-05-24): adds per-app `releases[]` (Release ledger
// per DESIGN-self-host-state.md §"The `Release` resource"). v2
// files migrate forward with Releases=nil — existing apps simply
// have no prior release history, which is the truthful state.
// Migration writes v3 back transparently.
const FormatVersion = 3

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
// bearing subset". Releases is the persisted Release ledger; nil
// or empty means no deploy has landed yet.
type App struct {
	Config   supervisor.Config `json:"config"`
	Metadata AppMetadata       `json:"metadata"`
	Releases []Release         `json:"releases,omitempty"`
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
	// ObservedGeneration is the Generation value the deploy flow
	// has converged to a healthy state on. In 0.0.x's synchronous
	// DeployApp, observedGeneration moves in lockstep with
	// Generation — AddApp writes them together. The async window
	// (creek deploy --watch: 202 + background goroutine) introduces
	// SetObservedGeneration as the late writer. The invariant
	// ObservedGeneration ≤ Generation is enforced by the setter
	// (monotonic guard — observedGeneration MUST NEVER decrease).
	ObservedGeneration int64 `json:"observed_generation"`
	// ResourceVersion bumps on every write (spec write via AddApp,
	// status write via SetObservedGeneration / SetConditions).
	// Served to clients as a string per K8s wire convention.
	// Clients MUST NOT do arithmetic on it.
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
//
// Concurrency model (#5b):
//
//   - locks: per-app RWMutexes exposed via Store.Locks() for callers
//     (CAS middleware + handlers) to hold across the
//     validate → If-Match → mutate → audit-WAL → flush sequence.
//     Different apps' write locks are independent so two mutations
//     against different apps proceed in parallel.
//
//   - flushMu: serialises the actual disk write step. Held briefly
//     during snapshot+marshal+rename+verify. Independent of per-app
//     locks so two mutations against different apps still queue at
//     the disk-write step (necessary — state.json is shared) but the
//     queue wait is microseconds-bounded, not seconds.
//
//   - memMu: protects the in-memory maps (apps, metadata, volumes)
//     so reads via Apps / Meta / Volumes never observe a torn swap.
//     Held briefly during snapshot at the start of each mutation and
//     during the swap at the end. Readers use RLock; multiple
//     concurrent readers proceed.
type Store struct {
	path string

	locks *LockManager

	flushMu sync.Mutex
	memMu   sync.RWMutex

	apps     map[string]supervisor.Config // appID → config
	metadata map[string]AppMetadata       // appID → envelope metadata
	releases map[string][]Release         // appID → release ledger (ordered by ReleaseSeq)
	volumes  map[string]supervisor.Volume // volumeID → volume
}

// Locks returns the per-app LockManager. CAS middleware acquires
// AppLock(id).Lock() before letting a mutation request through and
// releases when the response is written; this guarantees the
// validate → If-Match → mutate → flush sequence is atomic per app.
func (s *Store) Locks() *LockManager { return s.locks }

// newAppMetadata generates fresh envelope metadata for a brand-new
// app. UID is UUIDv7 (RFC 9562) — time-ordered. Generation starts at
// 1 (the create write counts); ResourceVersion starts at 1 likewise.
// CreationTimestamp is the moment of creation in UTC.
//
// Returns an error only if uuid.NewV7 fails, which only happens when
// the system can't produce 6 bytes of randomness — i.e. crypto/rand
// is broken. We deliberately do NOT fall back to v4 here: mixing v4
// and v7 in the same store breaks the time-ordering invariant that
// callers depend on (e.g. listing apps by UID approximates by-time).
// If the host can't provide entropy, refusing AddApp is the honest
// failure mode.
func newAppMetadata() (AppMetadata, error) {
	u, err := uuid.NewV7()
	if err != nil {
		return AppMetadata{}, fmt.Errorf("state: uuid v7: %w", err)
	}
	return AppMetadata{
		UID:                u.String(),
		Generation:         1,
		ObservedGeneration: 1, // sync spawn → convergence is atomic
		ResourceVersion:    1,
		CreationTimestamp:  time.Now().UTC(),
	}, nil
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
	if err := checkFilesystem(path); err != nil {
		return nil, err
	}
	s := &Store{
		path:     path,
		locks:    NewLockManager(),
		apps:     make(map[string]supervisor.Config),
		metadata: make(map[string]AppMetadata),
		releases: make(map[string][]Release),
		volumes:  make(map[string]supervisor.Volume),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	// WAL replay-forward: any pending record without a matching
	// commit captures a crash mid-write. Replay re-writes the
	// pending's full state.json payload then emits a commit so the
	// next boot sees nothing to do. See wal.go for the on-disk
	// shape.
	if err := s.replayWAL(); err != nil {
		return nil, err
	}
	return s, nil
}

// replayWAL scans the WAL for orphan pending records and re-applies
// the latest one's payload as state.json. After replay the
// in-memory maps are repopulated to match.
//
// Most boots find no orphans — fast path returns immediately. When
// orphans exist (crash recovery), the latest pending's payload
// reflects the most recent intended state (per-app mutex serialises
// same-app mutations; cross-app the LAST pending in the WAL is
// always the freshest cumulative snapshot since flushFull holds
// flushMu so only one mutation lands at a time).
func (s *Store) replayWAL() error {
	orphans, err := scanOrphanPending(walPath(s.path))
	if err != nil {
		return fmt.Errorf("state: scan WAL for replay: %w", err)
	}
	if len(orphans) == 0 {
		return nil
	}
	latest := orphans[len(orphans)-1]
	payload, err := decodePendingPayload(latest)
	if err != nil {
		return err
	}

	// Apply the payload as state.json via the same durability
	// sequence as flushFull, MINUS the WAL writes (we're recovering
	// FROM the WAL — re-appending pending would be a loop).
	dir := filepath.Dir(s.path)
	tmp := s.path + ".tmp"
	if err := writeFileAndFsync(tmp, payload, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: replay write+fsync: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: replay fsync parent (pre-rename): %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: replay rename: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("state: replay fsync parent (post-rename): %w", err)
	}

	// Repopulate in-memory maps from the replayed payload — load()
	// already ran above against the pre-crash state.json, so the
	// maps are stale. Drop them and re-decode from the new payload.
	s.apps = make(map[string]supervisor.Config)
	s.metadata = make(map[string]AppMetadata)
	s.releases = make(map[string][]Release)
	s.volumes = make(map[string]supervisor.Volume)
	if err := s.loadV2OrV3(payload); err != nil {
		return fmt.Errorf("state: replay decode payload: %w", err)
	}

	// Close the chain with a commit-after-replay record so the next
	// boot won't try to replay this token again.
	if err := appendCommit(walPath(s.path), latest.Token); err != nil {
		return fmt.Errorf("state: replay commit: %w", err)
	}
	return nil
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
	s.memMu.RLock()
	defer s.memMu.RUnlock()
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
	// flushMu serialises the snapshot+marshal+write+swap sequence
	// against other in-Store mutations. Per-app locks (Locks().AppLock)
	// are the upstream caller's responsibility — they gate the wider
	// validate → If-Match → mutate flow at handler scope.
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	// Snapshot under memMu.RLock so a concurrent reader (Apps / Meta)
	// observes a consistent map.
	s.memMu.RLock()
	nextApps := cloneMap(s.apps)
	nextMeta := cloneMetadataMap(s.metadata)
	existingMeta, hasMeta := s.metadata[cfg.ID]
	currentVolumes := s.volumes
	s.memMu.RUnlock()

	// Deep-clone the inbound config so a later mutation by the caller
	// (e.g. appending to cfg.Env) cannot reach back into our snapshot.
	nextApps[cfg.ID] = supervisor.CloneConfig(cfg)
	if hasMeta {
		// Deploy / overwrite: preserve identity, bump versions.
		// ObservedGeneration is bumped in lockstep with Generation
		// because 0.0.x's DeployApp is synchronous: by the time
		// AddApp lands, sup.Deploy has already converged. When #26
		// flips DeployApp to 202 + background, the path will be
		// "AddApp bumps Generation; SetObservedGeneration bumps
		// observedGen later"; for now the invariant is
		// Generation == ObservedGeneration on every successful Deploy.
		nextMeta[cfg.ID] = AppMetadata{
			UID:                existingMeta.UID,
			CreationTimestamp:  existingMeta.CreationTimestamp,
			Generation:         existingMeta.Generation + 1,
			ObservedGeneration: existingMeta.Generation + 1,
			ResourceVersion:    existingMeta.ResourceVersion + 1,
		}
	} else {
		meta, err := newAppMetadata()
		if err != nil {
			return err
		}
		nextMeta[cfg.ID] = meta
	}
	if err := s.flushFull(nextApps, nextMeta, currentVolumes); err != nil {
		return err
	}
	// Swap in atomically against any concurrent reader.
	s.memMu.Lock()
	s.apps = nextApps
	s.metadata = nextMeta
	s.memMu.Unlock()
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
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.memMu.RLock()
	if _, ok := s.apps[id]; !ok {
		s.memMu.RUnlock()
		return nil
	}
	nextApps := cloneMap(s.apps)
	nextMeta := cloneMetadataMap(s.metadata)
	nextReleases := cloneReleaseMap(s.releases)
	currentVolumes := s.volumes
	s.memMu.RUnlock()

	delete(nextApps, id)
	delete(nextMeta, id)
	delete(nextReleases, id)
	if err := s.flushFullWithReleases(nextApps, nextMeta, nextReleases, currentVolumes); err != nil {
		return err
	}
	s.memMu.Lock()
	s.apps = nextApps
	s.metadata = nextMeta
	s.releases = nextReleases
	s.memMu.Unlock()
	return nil
}

// Meta returns the persisted envelope metadata for id. The bool is
// false when no such app exists.
func (s *Store) Meta(id string) (AppMetadata, bool) {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
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
		// Persist forward as the current format so subsequent boots
		// skip the migration path. load() runs single-threaded during
		// NewStore — no concurrent access possible — but flushBoth
		// requires flushMu (PR #6's per-app-lock refactor separated
		// this from s.mu) so acquire it explicitly to honour the
		// contract.
		s.flushMu.Lock()
		err := s.flushBoth(s.apps, s.volumes)
		s.flushMu.Unlock()
		if err != nil {
			return fmt.Errorf("state: v1→v%d migration flush %s: %w", FormatVersion, s.path, err)
		}
		return nil
	case 2:
		if err := s.loadV2OrV3(data); err != nil {
			return err
		}
		// v2 → v3: no shape change beyond the new Releases field
		// (which is omitempty + nil on load). Persist forward as v3
		// transparently.
		s.flushMu.Lock()
		err := s.flushBoth(s.apps, s.volumes)
		s.flushMu.Unlock()
		if err != nil {
			return fmt.Errorf("state: v2→v3 migration flush %s: %w", s.path, err)
		}
		return nil
	case FormatVersion:
		return s.loadV2OrV3(data)
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
		meta, err := newAppMetadata()
		if err != nil {
			return err
		}
		s.metadata[a.Config.ID] = meta
	}
	return nil
}

// loadV2OrV3 decodes the current State shape. v2 and v3 are
// structurally identical except for the Releases field (omitempty
// on v2 reads → nil slice, which is correct). Same decoder serves
// both.
func (s *Store) loadV2OrV3(data []byte) error {
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return fmt.Errorf("state: decode v2/v3 %s: %w", s.path, err)
	}
	for _, v := range st.Volumes {
		if v.Volume.ID == "" {
			continue
		}
		s.volumes[v.Volume.ID] = v.Volume
	}
	var repaired bool
	for _, a := range st.Apps {
		if a.Config.ID == "" {
			continue
		}
		s.apps[a.Config.ID] = a.Config
		// Defensive: if a v2/v3 file is missing metadata (shouldn't
		// happen with our writer, but a hand-edited file might),
		// fall back to a freshly-generated stamp.
		if a.Metadata.UID == "" {
			meta, err := newAppMetadata()
			if err != nil {
				return err
			}
			s.metadata[a.Config.ID] = meta
			repaired = true
		} else {
			s.metadata[a.Config.ID] = a.Metadata
		}
		if len(a.Releases) > 0 {
			clone := make([]Release, len(a.Releases))
			copy(clone, a.Releases)
			s.releases[a.Config.ID] = clone
		}
	}
	// Persist any repaired metadata back to disk. Without this, every
	// restart would regenerate the UID — violating the contract that
	// uid + creationTimestamp are never regenerated across restore.
	// Mirrors the v1→v2 migration flush in load().
	if repaired {
		s.flushMu.Lock()
		err := s.flushBoth(s.apps, s.volumes)
		s.flushMu.Unlock()
		if err != nil {
			return fmt.Errorf("state: persist repaired v2 metadata %s: %w", s.path, err)
		}
	}
	return nil
}

// Volumes returns a snapshot of every persisted Volume in
// alphabetical order. Replayed at daemon start before any Apps so
// Spawn's VolumeMount references resolve.
func (s *Store) Volumes() []supervisor.Volume {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
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
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.memMu.RLock()
	nextVolumes := cloneVolumeMap(s.volumes)
	currentApps := s.apps
	s.memMu.RUnlock()

	nextVolumes[v.ID] = v
	if err := s.flushBoth(currentApps, nextVolumes); err != nil {
		return err
	}
	s.memMu.Lock()
	s.volumes = nextVolumes
	s.memMu.Unlock()
	return nil
}

// RemoveVolume drops the entry for id. Unknown id is a no-op.
func (s *Store) RemoveVolume(id string) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.memMu.RLock()
	if _, ok := s.volumes[id]; !ok {
		s.memMu.RUnlock()
		return nil
	}
	nextVolumes := cloneVolumeMap(s.volumes)
	currentApps := s.apps
	s.memMu.RUnlock()

	delete(nextVolumes, id)
	if err := s.flushBoth(currentApps, nextVolumes); err != nil {
		return err
	}
	s.memMu.Lock()
	s.volumes = nextVolumes
	s.memMu.Unlock()
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
// migration writeback path. Caller must hold s.flushMu.
//
// Reads s.metadata under memMu.RLock — Volume mutations don't change
// the app metadata map but the read still needs synchronisation
// against concurrent AddApp/RemoveApp swaps.
func (s *Store) flushBoth(apps map[string]supervisor.Config, volumes map[string]supervisor.Volume) error {
	s.memMu.RLock()
	metaSnapshot := s.metadata
	s.memMu.RUnlock()
	return s.flushFull(apps, metaSnapshot, volumes)
}

// flushFull writes all three maps atomically with the full
// durability + WAL sequence:
//
//  1. Marshal target state, hash the bytes.
//  2. Append a pending record to <path>.wal with the bytes + hash;
//     fsync the WAL.
//  3. Write temp file → fsync(temp) so contents reach disk.
//  4. Rename temp → state.json (atomic on ext4/xfs).
//  5. fsync(parent_dir) so the directory entry update reaches disk.
//  6. Read-back verify: re-read state.json, hash it, compare to (1).
//     A mismatch indicates a silent fsync EIO (Postgres "fsyncgate"
//     pattern) and surfaces StorageCorruptedError so the daemon
//     can refuse further writes rather than building on rotten
//     bytes.
//  7. Append a commit record to the WAL referencing the pending
//     token; fsync the WAL.
//
// Crash anywhere between (2)'s fsync and (7)'s fsync produces an
// orphan pending without commit. Boot replay-forward (in load())
// detects orphans and re-writes their payload as state.json — the
// pending captured the full bytes that should land, so replay is
// deterministic.
//
// Caller must hold s.flushMu.
//
// Releases are sourced from the Store's in-memory snapshot at
// call time. Mutations to the release ledger (CreateRelease)
// have their own flushFullWithReleases entry point that builds
// the next-releases map; bare flushFull preserves the existing
// ledger.
func (s *Store) flushFull(apps map[string]supervisor.Config, metadata map[string]AppMetadata, volumes map[string]supervisor.Volume) error {
	s.memMu.RLock()
	releasesSnapshot := s.releases
	s.memMu.RUnlock()
	return s.flushFullWithReleases(apps, metadata, releasesSnapshot, volumes)
}

// flushFullWithReleases is the underlying durability path,
// accepting the full state quad (apps, metadata, releases,
// volumes). flushFull wraps it for callers that don't touch the
// release ledger; CreateRelease calls it directly with the
// staged next-releases map.
func (s *Store) flushFullWithReleases(apps map[string]supervisor.Config, metadata map[string]AppMetadata, releases map[string][]Release, volumes map[string]supervisor.Volume) error {
	// Mode 0o700 / 0o600 — state.json contains every supervised
	// app's full Config (env vars, commands, volume refs). World-
	// readable defaults would leak tenant secrets to any local
	// process. Pentest H6 surfaced the previous 0o755/0o644.
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
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
			Releases: releases[id],
		})
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	wantHash := sha256.Sum256(data)
	wantHex := hex.EncodeToString(wantHash[:])

	// WAL pending FIRST — its fsync makes durable the intent to
	// land the new state. A subsequent crash (kernel panic, power
	// loss, kill -9) triggers boot replay-forward of this token.
	// A subsequent SOFT failure (read-only directory, fsync EIO)
	// is recovered by emitting a rollback so the next boot treats
	// the pending as a no-op rather than materialising an intent
	// the caller observed as rejected.
	walFile := walPath(s.path)
	token, err := appendPending(walFile, data)
	if err != nil {
		return fmt.Errorf("state: wal pending: %w", err)
	}

	// Anchor for the rollback-on-error path. Set rollbackOnErr =
	// false right before returning success so the deferred rollback
	// is a no-op on the happy path.
	//
	// Suppressed for StorageCorruptedError: when read-back verify
	// fails we DON'T know whether the bytes on disk match `data` or
	// not — fsync may have silently failed. Emitting a rollback would
	// tell next-boot replay "this pending is closed", causing it to
	// skip replaying the intended state. Leaving the pending orphan
	// preserves the option to replay-forward.
	rollbackOnErr := true
	suppressRollback := false
	defer func() {
		if rollbackOnErr && !suppressRollback {
			// Best-effort rollback. If the WAL itself is broken
			// here (e.g. disk full) the next boot will see an
			// orphan pending and replay it — same outcome the
			// pre-rollback DESIGN intended. Either way the daemon
			// state stays consistent with on-disk reality after
			// the next boot.
			_ = appendRollback(walFile, token)
		}
	}()

	tmp := s.path + ".tmp"
	if err := writeFileAndFsync(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: write+fsync %s: %w", tmp, err)
	}
	// fsync parent dir BEFORE rename so the tmp file's directory entry
	// is durable. On ext4/xfs a freshly-created file's dirent isn't
	// guaranteed durable until the directory itself is synced — without
	// this, a crash between writeFileAndFsync and rename could leave the
	// tmp file's contents on disk without an entry pointing to them.
	if err := fsyncDir(dir); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: fsync parent %s (pre-rename): %w", dir, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename %s → %s: %w", tmp, s.path, err)
	}
	// fsync parent dir AFTER rename so the rename itself is durable.
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("state: fsync parent %s (post-rename): %w", dir, err)
	}

	// Read-back verification. If the kernel silently dropped a dirty
	// page after fsync returned (the well-known Postgres fsyncgate
	// pattern on ext4), the on-disk hash won't match what we wrote.
	// Detecting it here keeps the daemon honest about durability.
	gotData, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("state: read-back %s: %w", s.path, err)
	}
	gotHash := sha256.Sum256(gotData)
	gotHex := hex.EncodeToString(gotHash[:])
	if gotHex != wantHex {
		suppressRollback = true
		return &StorageCorruptedError{
			Path:       s.path,
			WantSHA256: wantHex,
			GotSHA256:  gotHex,
		}
	}

	// WAL commit LAST — confirms the new state landed durably. Any
	// next-boot scan that finds this token committed knows it can
	// skip replay. fsync ensures we don't observe a "rolled back"
	// commit later via Linux's writeback cache.
	if err := appendCommit(walFile, token); err != nil {
		return fmt.Errorf("state: wal commit: %w", err)
	}
	rollbackOnErr = false
	return nil
}

// writeFileAndFsync creates path (truncate if exists), writes data,
// fsyncs the file descriptor, then closes. Mirrors os.WriteFile +
// explicit fsync that os.WriteFile leaves to the kernel's whim.
func writeFileAndFsync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if err := writeAll(f, data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// writeAll writes data to f and returns an error if Write fails or
// short-writes. os.File.Write may return a short write with nil
// error per the io.Writer contract; treating n != len(data) as an
// error closes the silent-corruption path.
func writeAll(f io.Writer, data []byte) error {
	n, err := f.Write(data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write %d of %d bytes", n, len(data))
	}
	return nil
}

// fsyncDir opens the directory and fsyncs it. On ext4/xfs this is
// what makes the rename's directory entry durable across power
// loss. On darwin (dev path) it's a no-op syscall — F_FULLFSYNC
// would be more strict but the dev Mac is never the production
// target.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
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
