package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/solcreek/creekd/internal/supervisor"
)

// TestWAL_PendingCommitPairLeavesNoOrphan covers the normal flush
// path: pending followed by commit should produce no orphan.
func TestWAL_PendingCommitPairLeavesNoOrphan(t *testing.T) {
	dir := t.TempDir()
	wp := filepath.Join(dir, "wal.log")
	tok, err := appendPending(wp, []byte(`{"version":2,"apps":[]}`))
	if err != nil {
		t.Fatalf("appendPending: %v", err)
	}
	if err := appendCommit(wp, tok); err != nil {
		t.Fatalf("appendCommit: %v", err)
	}
	orphans, err := scanOrphanPending(wp)
	if err != nil {
		t.Fatalf("scanOrphanPending: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphan count = %d, want 0 (pending+commit pair)", len(orphans))
	}
}

// TestWAL_PendingWithoutCommitIsOrphan covers the crash case: pending
// fsync'd, then crash before commit → scan returns the pending as
// orphan.
func TestWAL_PendingWithoutCommitIsOrphan(t *testing.T) {
	dir := t.TempDir()
	wp := filepath.Join(dir, "wal.log")
	if _, err := appendPending(wp, []byte(`{"version":2,"apps":[]}`)); err != nil {
		t.Fatalf("appendPending: %v", err)
	}
	orphans, err := scanOrphanPending(wp)
	if err != nil {
		t.Fatalf("scanOrphanPending: %v", err)
	}
	if len(orphans) != 1 {
		t.Errorf("orphan count = %d, want 1", len(orphans))
	}
}

// TestWAL_PendingRollbackPairLeavesNoOrphan covers the soft-failure
// recovery: pending fsync'd, daemon emits rollback on error → scan
// treats it the same as a commit.
func TestWAL_PendingRollbackPairLeavesNoOrphan(t *testing.T) {
	dir := t.TempDir()
	wp := filepath.Join(dir, "wal.log")
	tok, err := appendPending(wp, []byte(`{"version":2,"apps":[]}`))
	if err != nil {
		t.Fatalf("appendPending: %v", err)
	}
	if err := appendRollback(wp, tok); err != nil {
		t.Fatalf("appendRollback: %v", err)
	}
	orphans, err := scanOrphanPending(wp)
	if err != nil {
		t.Fatalf("scanOrphanPending: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphan count = %d, want 0 (pending+rollback pair)", len(orphans))
	}
}

// TestWAL_DecodePendingPayloadVerifiesHash covers WAL file
// corruption: a pending record with mismatched hash + payload must
// surface WALHashMismatchError on decode.
func TestWAL_DecodePendingPayloadVerifiesHash(t *testing.T) {
	dir := t.TempDir()
	wp := filepath.Join(dir, "wal.log")
	if _, err := appendPending(wp, []byte(`{"version":2,"apps":[]}`)); err != nil {
		t.Fatalf("appendPending: %v", err)
	}
	// Read the record back, tamper with the payload, try decode.
	orphans, err := scanOrphanPending(wp)
	if err != nil {
		t.Fatalf("scanOrphanPending: %v", err)
	}
	if len(orphans) != 1 {
		t.Fatalf("expected exactly 1 orphan, got %d", len(orphans))
	}
	rec := orphans[0]
	// Tamper: replace the base64 payload with a different valid
	// base64 (just take the original's encoding of a DIFFERENT
	// string).
	rec.StatePayloadB64 = "Y29ycnVwdGVk" // "corrupted" in base64
	_, err = decodePendingPayload(rec)
	var hashErr *WALHashMismatchError
	if !errors.As(err, &hashErr) {
		t.Errorf("decodePendingPayload on tampered record: err = %v, want WALHashMismatchError", err)
	}
}

// TestWAL_BootReplayRecoversFromCrashBetweenPendingAndCommit
// simulates the crash window between pending fsync and commit fsync
// directly at the WAL primitive layer:
//
//   1. Build a Store, do one normal AddApp → wal has [P1, C1].
//   2. Hand-inject a pending record into the WAL (no commit) that
//      represents a "next intended state".
//   3. Open a fresh Store. replayWAL should pick up the orphan and
//      re-apply its payload as state.json.
//   4. The reloaded Store's in-memory state should reflect the
//      replayed payload, not the pre-crash state.json.
func TestWAL_BootReplayRecoversFromCrashBetweenPendingAndCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Phase 1: normal AddApp lands [first].
	s1, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore #1: %v", err)
	}
	if err := s1.AddApp(supervisor.Config{ID: "first", Command: "sleep", Args: []string{"1"}, Port: 9000}); err != nil {
		t.Fatalf("AddApp first: %v", err)
	}

	// Phase 2: hand-craft a state-with-pending-applied that would be
	// the in-flight mutation at crash time. We pretend the next mutation
	// would have added "crashed". Build the state.json bytes that this
	// mutation WOULD have written, append as a pending into WAL — but
	// DO NOT actually update state.json on disk (simulating a crash
	// between pending fsync and rename).
	stateWithCrashed := State{
		Version: FormatVersion,
		Apps: []App{
			s1Apps(t, s1)[0], // existing "first" entry
			{Config: supervisor.Config{ID: "crashed", Command: "sleep", Args: []string{"2"}, Port: 9001},
				Metadata: AppMetadata{
					UID:               "00000000-0000-7000-8000-000000000001",
					Generation:        1,
					ResourceVersion:   1,
					CreationTimestamp: s1Apps(t, s1)[0].Metadata.CreationTimestamp,
				}},
		},
	}
	payload, err := marshalState(stateWithCrashed)
	if err != nil {
		t.Fatalf("marshal crashed state: %v", err)
	}
	if _, err := appendPending(walPath(path), payload); err != nil {
		t.Fatalf("manually appendPending: %v", err)
	}

	// Phase 3: open a fresh Store. replayWAL should fire.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore #2 (replay): %v", err)
	}

	// Phase 4: the new Store should see BOTH "first" and "crashed".
	got := s2.Apps()
	if len(got) != 2 {
		t.Fatalf("post-replay app count = %d, want 2; got = %+v", len(got), got)
	}
	ids := []string{got[0].ID, got[1].ID}
	if !(contains(ids, "first") && contains(ids, "crashed")) {
		t.Errorf("post-replay ids = %v; want both \"first\" and \"crashed\"", ids)
	}

	// Phase 5: orphan should be gone now (replay closed it with a
	// commit-after-replay record).
	orphans, err := scanOrphanPending(walPath(path))
	if err != nil {
		t.Fatalf("scanOrphanPending post-replay: %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans after replay = %d, want 0 (replay should emit commit-after-replay)", len(orphans))
	}
}

// TestWAL_FailedFlushDoesNotMaterialiseOnNextBoot is the integration
// test for the rollback path: a flushFull that errors after pending
// fsync (here forced via read-only directory) must NOT cause the
// next boot to replay the pending. Pre-rollback behaviour would
// have materialised the failed intent.
func TestWAL_FailedFlushDoesNotMaterialiseOnNextBoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX write-mode bits; cannot force flush failure")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.AddApp(supervisor.Config{ID: "kept", Command: "sleep", Args: []string{"1"}, Port: 8000}); err != nil {
		t.Fatalf("AddApp kept: %v", err)
	}

	// Force the next flush to fail.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := s.AddApp(supervisor.Config{ID: "rejected", Command: "sleep", Args: []string{"1"}, Port: 8001}); err == nil {
		t.Fatal("AddApp should fail on read-only dir")
	}

	// Restore directory permissions so NewStore can proceed.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod rw: %v", err)
	}

	// Open a fresh Store. The orphan pending for "rejected" should
	// NOT replay — the daemon's deferred rollback in flushFull's
	// error path closed it.
	s2, err := NewStore(path)
	if err != nil {
		t.Fatalf("NewStore (after failed flush): %v", err)
	}
	apps := s2.Apps()
	if len(apps) != 1 || apps[0].ID != "kept" {
		t.Errorf("after recovery, apps = %+v; want only \"kept\" (rejected must NOT materialise)", apps)
	}
}

// TestWAL_AppendModeAllowsRecoveryAfterRollback covers the boundary
// where a rollback writes successfully into the WAL even when the
// state dir is read-only — because the WAL FILE already exists,
// O_APPEND on it doesn't need dir-create permission.
func TestWAL_AppendModeAllowsRecoveryAfterRollback(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses POSIX write-mode bits")
	}
	dir := t.TempDir()
	wp := filepath.Join(dir, "wal.log")
	tok, err := appendPending(wp, []byte(`{"version":2,"apps":[]}`))
	if err != nil {
		t.Fatalf("setup appendPending: %v", err)
	}
	// Make the dir read-only — this would block O_CREATE but not
	// O_APPEND on an existing file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := appendRollback(wp, tok); err != nil {
		t.Errorf("appendRollback on existing WAL file in read-only dir: %v", err)
	}
}

// Helpers -------------------------------------------------------------

// s1Apps reconstructs the {Config, Metadata} pairs from a Store so
// tests can hand-craft a State value that round-trips through
// loadV2.
func s1Apps(t *testing.T, s *Store) []App {
	t.Helper()
	apps := s.Apps()
	out := make([]App, len(apps))
	for i, cfg := range apps {
		meta, _ := s.Meta(cfg.ID)
		out[i] = App{Config: cfg, Metadata: meta}
	}
	return out
}

// marshalState matches flushFull's encoding (json.MarshalIndent with
// two-space indent) so the bytes can be decoded by loadV2.
func marshalState(st State) ([]byte, error) {
	return json.MarshalIndent(st, "", "  ")
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
