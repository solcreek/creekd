package supervisor

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Volume is a supervisor-level declared filesystem resource. It
// represents a host-side directory that one or more apps may
// project into their own filesystem view via VolumeMount.
//
// The two-layer split (Volume vs VolumeMount) exists to make
// per-tenant data lifecycle independent of any one app's lifecycle:
//
//   - Tenant volumes survive app crashes, supervisor restarts, and
//     blue/green deploys. Lifecycle is "operator decommissioning a
//     tenant", not "process exiting."
//
//   - Per-app projection (VolumeMount) is cheap to create and tear
//     down. Multiple apps in the same tenant (e.g. Postgres primary
//     + pgBackRest cron) reference the same Volume by ID.
//
// Volumes are anchored under Supervisor.VolumeRoot. Resolution uses
// openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS|RESOLVE_NO_MAGICLINKS)
// from a pinned O_PATH fd of VolumeRoot — caller-supplied
// BackingPath strings cannot escape the tenant subtree via symlink
// games, "..", or TOCTOU.
type Volume struct {
	// ID is the stable identifier referenced by VolumeMount.VolumeID.
	// Must match ValidateID grammar (same rules as app IDs).
	ID string

	// BackingPath is the on-host directory, relative to
	// Supervisor.VolumeRoot. Absolute paths and paths containing
	// ".." are rejected. The directory must already exist —
	// creekd does not create or destroy tenant data dirs (that's
	// orchestrator policy).
	BackingPath string

	// ReadOnly is the default projection mode. VolumeMount.ReadOnly
	// can promote a RW Volume to RO for a single app; the reverse
	// (RO Volume, RW projection) is rejected.
	ReadOnly bool

	// FSType is discovered at registration via statfs and recorded
	// for observability / future quota wiring. Not user-supplied.
	FSType string

	// resolvedAbsPath is the kernel-resolved absolute path captured
	// from /proc/self/fd/N of the openat2 fd at registration time.
	// Stored so per-spawn identity checks compare against a frozen
	// truth instead of re-resolving a string. Unexported because
	// it's not part of the on-disk persistence schema — it's
	// re-derived at restore.
	resolvedAbsPath string `json:"-"`

	// devMajor / devMinor / inode together identify the backing
	// directory at the moment of registration. Used by
	// isAlreadyBoundExact to detect "stale source via inode reuse"
	// and to make cross-tenant collisions on the same filesystem
	// distinguishable (st_dev alone is not enough — same FS shares
	// st_dev). All zero when never registered.
	devMajor uint32 `json:"-"`
	devMinor uint32 `json:"-"`
	inode    uint64 `json:"-"`
}

// Errors returned by the Volume API.
var (
	ErrVolumeRootRequired   = errors.New("supervisor: VolumeRoot must be configured before registering volumes")
	ErrVolumeNotFound       = errors.New("volume not found")
	ErrVolumeAlreadyExists  = errors.New("volume already registered")
	ErrVolumeInUse          = errors.New("volume has active projections; cannot unregister")
	ErrVolumeBackingMissing = errors.New("volume backing path does not exist or is not a directory")
)

// RegisterVolume declares a Volume so it can be referenced by
// VolumeMount.VolumeID. Side effects:
//
//   - Resolves BackingPath safely via openat2 anchored at VolumeRoot.
//   - Marks the resolved path MS_PRIVATE to break shared-propagation
//     leakage. (Linux only.)
//   - Records the filesystem type via statfs for future quota wiring.
//
// Idempotent: re-registering the same ID with identical BackingPath
// and ReadOnly is a no-op. Re-registering with different values
// returns ErrVolumeAlreadyExists — operator must Unregister first.
func (s *Supervisor) RegisterVolume(v Volume) error {
	if err := ValidateID(v.ID); err != nil {
		return fmt.Errorf("supervisor: volume id: %w", err)
	}
	if s.VolumeRoot == "" {
		return ErrVolumeRootRequired
	}
	if v.BackingPath == "" {
		return errors.New("supervisor: volume backing_path is required")
	}
	if filepath.IsAbs(v.BackingPath) {
		return fmt.Errorf("supervisor: volume backing_path %q must be relative to VolumeRoot", v.BackingPath)
	}
	if containsDotDot(v.BackingPath) {
		return fmt.Errorf("supervisor: volume backing_path %q contains '..'", v.BackingPath)
	}
	if strings.HasPrefix(v.BackingPath, "/") {
		return fmt.Errorf("supervisor: volume backing_path %q must not start with '/'", v.BackingPath)
	}

	s.volumesMu.Lock()
	defer s.volumesMu.Unlock()
	if cur, ok := s.volumes[v.ID]; ok {
		if cur.BackingPath == filepath.Clean(v.BackingPath) && cur.ReadOnly == v.ReadOnly {
			return nil
		}
		return fmt.Errorf("supervisor: %q: %w", v.ID, ErrVolumeAlreadyExists)
	}

	resolved, fsType, err := s.openAndIsolateVolume(filepath.Clean(v.BackingPath))
	if err != nil {
		return err
	}
	resolved.ID = v.ID
	resolved.ReadOnly = v.ReadOnly
	resolved.FSType = fsType
	s.volumes[v.ID] = resolved
	s.logger.Info("volume registered",
		"id", v.ID,
		"backing_path", resolved.BackingPath,
		"fs_type", resolved.FSType,
		"read_only", resolved.ReadOnly,
	)
	return nil
}

// UnregisterVolume removes a Volume from the registry. Refuses to
// remove a Volume that any registered app still references via
// VolumeMounts, unless force is true. UnregisterVolume does NOT
// delete the on-disk backing data — that's orchestrator policy.
//
// The refcount walks every App's volumeRefs slice (frozen at Spawn).
// Without this, the previous implementation's "in-use" check was
// dead code — pentest review confirmed both errVolumeInUse and
// force=false would silently allow removal of an in-use volume.
func (s *Supervisor) UnregisterVolume(id string, force bool) error {
	s.volumesMu.Lock()
	defer s.volumesMu.Unlock()
	if _, ok := s.volumes[id]; !ok {
		return fmt.Errorf("supervisor: %q: %w", id, ErrVolumeNotFound)
	}
	if !force {
		refs := s.appsReferencingVolume(id)
		if len(refs) > 0 {
			return fmt.Errorf("supervisor: volume %q still referenced by %d app(s): %v: %w",
				id, len(refs), refs, ErrVolumeInUse)
		}
	}
	delete(s.volumes, id)
	s.logger.Info("volume unregistered", "id", id, "force", force)
	return nil
}

// appsReferencingVolume returns the IDs of every registered app
// whose Config.VolumeMounts references volumeID. Caller must hold
// volumesMu (this method grabs the apps lock).
func (s *Supervisor) appsReferencingVolume(volumeID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var refs []string
	for appID, app := range s.apps {
		for _, ref := range app.volumeRefs {
			if ref == volumeID {
				refs = append(refs, appID)
				break
			}
		}
	}
	return refs
}

// volumeIDs extracts the set of volume IDs referenced by a slice of
// VolumeMounts. Used by Spawn to record App.volumeRefs.
func volumeIDs(mounts []VolumeMount) []string {
	if len(mounts) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(mounts))
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		if _, dup := seen[m.VolumeID]; dup {
			continue
		}
		seen[m.VolumeID] = struct{}{}
		out = append(out, m.VolumeID)
	}
	return out
}

// Volume returns a snapshot of the named Volume.
func (s *Supervisor) Volume(id string) (Volume, bool) {
	s.volumesMu.RLock()
	defer s.volumesMu.RUnlock()
	v, ok := s.volumes[id]
	if !ok {
		return Volume{}, false
	}
	return *v, true
}

// Volumes returns a snapshot of every registered Volume, in
// alphabetical ID order.
func (s *Supervisor) Volumes() []Volume {
	s.volumesMu.RLock()
	defer s.volumesMu.RUnlock()
	out := make([]Volume, 0, len(s.volumes))
	for _, v := range s.volumes {
		out = append(out, *v)
	}
	// In-place insertion sort by ID; len ≤ a few hundred in
	// realistic deployments.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// containsDotDot reports whether any cleaned path segment is "..".
// Matches the check used in net/http.ServeFile and is the load-bearing
// guard against traversal through caller-supplied path strings — the
// kernel-level guard is openat2(RESOLVE_BENEATH), this is the cheap
// pre-check.
func containsDotDot(p string) bool {
	if p == "" {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
