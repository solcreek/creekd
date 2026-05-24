package state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/solcreek/creekd/internal/supervisor"
)

// EnvHash returns "sha256:" + hex over the sorted KEY=value
// pairs of env. Used as the ReleaseSpec.EnvHash field so a
// rollback can detect identical-vs-divergent env without
// inspecting the values themselves. Order-insensitive: the hash
// only depends on the SET of pairs, not their order in the input
// slice.
func EnvHash(env []string) string {
	if len(env) == 0 {
		return ""
	}
	sorted := append([]string(nil), env...)
	sort.Strings(sorted)
	h := sha256.New()
	h.Write([]byte(strings.Join(sorted, "\x00")))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Release is the persisted ledger entry created on every deploy
// (and every rollback — see CreateRelease's PriorActivePhase
// parameter). Records are kept indefinitely; only the underlying
// artifact images are garbage-collected per
// DESIGN-self-host-state.md §"Storage / GC".
//
// Release is immutable except for Phase, which transitions
// Active → Superseded | RolledBack as newer releases land. The
// at-most-one-Active-per-app invariant is enforced by
// Store.CreateRelease as a single WAL-batched mutation.
type Release struct {
	UID               string       `json:"uid"`
	Phase             ReleasePhase `json:"phase"`
	CreationTimestamp time.Time    `json:"creation_timestamp"`
	Spec              ReleaseSpec  `json:"spec"`
}

// ReleaseSpec is the immutable payload of a Release. Once
// persisted no field here changes. Pointers (RolledBackFrom,
// OriginalArtifactRelease) reference other Releases by seq within
// the same app; zero means "not applicable" (fresh deploy, not a
// rollback).
//
// ConfigSnapshot pins the supervisor.Config at release-creation
// time so a future rollback can re-run the exact same workload.
// 0.0.x has no image registry — the Config snapshot IS the
// release artifact. 0.1.0 may layer image references on top but
// keeps the snapshot for in-tree rollback semantics.
type ReleaseSpec struct {
	AppUID                  string             `json:"app_uid"`
	ReleaseSeq              int64              `json:"release_seq"`
	GitSha                  string             `json:"git_sha,omitempty"`
	Image                   string             `json:"image,omitempty"`
	EnvHash                 string             `json:"env_hash,omitempty"`
	CreatedBy               string             `json:"created_by,omitempty"`
	RolledBackFrom          int64              `json:"rolled_back_from,omitempty"`
	OriginalArtifactRelease int64              `json:"original_artifact_release,omitempty"`
	ConfigSnapshot          *supervisor.Config `json:"config_snapshot,omitempty"`
}

// ReleasePhase is the closed enum of mutable phase values per
// DESIGN. Fresh releases land Active; the prior Active flips to
// either Superseded (a newer deploy supersedes it) or RolledBack
// (a rollback moved away from it).
type ReleasePhase string

const (
	ReleasePhaseActive     ReleasePhase = "Active"
	ReleasePhaseSuperseded ReleasePhase = "Superseded"
	ReleasePhaseRolledBack ReleasePhase = "RolledBack"
)

// ReleaseInput is the caller-supplied portion of a new Release.
// ReleaseSeq, UID, CreationTimestamp, and Phase are assigned by
// Store.CreateRelease; AppUID is read from the store's persisted
// metadata.
//
// PriorActivePhase controls how the existing Active release (if
// any) transitions. The zero value is treated as Superseded —
// fresh deploys leave this empty; rollback callers set it to
// RolledBack.
type ReleaseInput struct {
	Spec             ReleaseSpec
	PriorActivePhase ReleasePhase
}

// ErrAppNotFound is returned by CreateRelease when appID has no
// persisted entry. Sentinel so callers can distinguish a "no such
// app" error from a "release storage failure" error.
var ErrAppNotFound = errors.New("state: app not found")

// CreateRelease persists a new Release for appID. The new release
// becomes Active; any existing Active release flips to
// in.PriorActivePhase (default Superseded). releaseSeq is the
// monotonically-incremented per-app counter — never reused, even
// after rollback.
//
// Write path: a single WAL pending record captures both
// mutations (append + prior-active flip) so boot replay either
// commits both or neither, preserving the at-most-one-Active
// invariant under crash. This matches DESIGN-self-host-state.md
// §"Release ledger atomicity".
//
// On success returns the newly-created Release (with assigned seq
// + uid + timestamp). The caller (admin handlers in #8c) is
// responsible for building the Spec — including
// RolledBackFrom + OriginalArtifactRelease — and for choosing
// PriorActivePhase.
func (s *Store) CreateRelease(appID string, in ReleaseInput) (Release, error) {
	if appID == "" {
		return Release{}, errors.New("state: empty app id")
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.memMu.RLock()
	meta, hasMeta := s.metadata[appID]
	if _, hasApp := s.apps[appID]; !hasApp {
		s.memMu.RUnlock()
		return Release{}, fmt.Errorf("%w: %s", ErrAppNotFound, appID)
	}
	nextApps := cloneMap(s.apps)
	nextMeta := cloneMetadataMap(s.metadata)
	nextReleases := cloneReleaseMap(s.releases)
	currentVolumes := s.volumes
	s.memMu.RUnlock()

	if !hasMeta {
		// Should never happen — apps and metadata are written in lock-
		// step — but guard against a hand-edited state.json missing
		// metadata.
		return Release{}, fmt.Errorf("state: app %s has no metadata", appID)
	}

	priorPhase := in.PriorActivePhase
	if priorPhase == "" {
		priorPhase = ReleasePhaseSuperseded
	}
	if priorPhase != ReleasePhaseSuperseded && priorPhase != ReleasePhaseRolledBack {
		return Release{}, fmt.Errorf("state: invalid PriorActivePhase %q", priorPhase)
	}

	existing := append([]Release(nil), nextReleases[appID]...)
	nextSeq := int64(1)
	for i := range existing {
		if existing[i].Phase == ReleasePhaseActive {
			existing[i].Phase = priorPhase
		}
		if existing[i].Spec.ReleaseSeq >= nextSeq {
			nextSeq = existing[i].Spec.ReleaseSeq + 1
		}
	}

	u, err := uuid.NewV7()
	if err != nil {
		u = uuid.New() // identical fallback as newAppMetadata
	}
	spec := in.Spec
	spec.AppUID = meta.UID
	spec.ReleaseSeq = nextSeq

	rel := Release{
		UID:               u.String(),
		Phase:             ReleasePhaseActive,
		CreationTimestamp: time.Now().UTC(),
		Spec:              spec,
	}
	existing = append(existing, rel)
	nextReleases[appID] = existing

	// Bump ResourceVersion (status side-effect; new release IS a
	// status mutation in K8s terms). Generation is unchanged — the
	// app's spec didn't move.
	nextMeta[appID] = AppMetadata{
		UID:               meta.UID,
		Generation:        meta.Generation,
		ResourceVersion:   meta.ResourceVersion + 1,
		CreationTimestamp: meta.CreationTimestamp,
	}

	if err := s.flushFullWithReleases(nextApps, nextMeta, nextReleases, currentVolumes); err != nil {
		return Release{}, err
	}

	s.memMu.Lock()
	s.apps = nextApps
	s.metadata = nextMeta
	s.releases = nextReleases
	s.memMu.Unlock()
	return rel, nil
}

// Releases returns the persisted release ledger for appID in
// creation order (== ascending ReleaseSeq). Returns nil for
// unknown apps; caller MAY treat nil + non-nil-empty as
// interchangeable.
func (s *Store) Releases(appID string) []Release {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	src := s.releases[appID]
	if len(src) == 0 {
		return nil
	}
	out := make([]Release, len(src))
	copy(out, src)
	return out
}

// ActiveRelease returns the (single) Active release for appID.
// The bool is false when there is no Active record — either no
// deploy has landed yet, or the last one was followed by a
// delete that purged the ledger.
func (s *Store) ActiveRelease(appID string) (Release, bool) {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	for _, r := range s.releases[appID] {
		if r.Phase == ReleasePhaseActive {
			return r, true
		}
	}
	return Release{}, false
}

// FindRelease returns the release with the given seq for appID.
// Used by rollback handlers to resolve --to=N references.
func (s *Store) FindRelease(appID string, seq int64) (Release, bool) {
	s.memMu.RLock()
	defer s.memMu.RUnlock()
	for _, r := range s.releases[appID] {
		if r.Spec.ReleaseSeq == seq {
			return r, true
		}
	}
	return Release{}, false
}

// cloneReleaseMap is the release-ledger counterpart to
// cloneMap / cloneMetadataMap. Each app's slice is copied so the
// flush staging area can be mutated without disturbing the
// active in-memory snapshot.
func cloneReleaseMap(src map[string][]Release) map[string][]Release {
	dst := make(map[string][]Release, len(src)+1)
	for k, v := range src {
		clone := make([]Release, len(v))
		copy(clone, v)
		dst[k] = clone
	}
	return dst
}
