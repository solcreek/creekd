package state

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/google/uuid"
)

// walPath returns the WAL file path for a given state.json path.
// Convention: <state.json>.wal, side-by-side so both are inside the
// same data directory and get the same filesystem-level guarantees.
func walPath(statePath string) string { return statePath + ".wal" }

// WAL record type constants. NDJSON discriminator.
//
// pending → state.json change is INTENDED; bytes attached.
// commit  → state.json change LANDED; pending's token is closed.
// rollback → state.json change was REVOKED by the daemon before
//            landing (e.g. flushFull errored after pending fsync
//            due to a soft failure like read-only filesystem).
//            Same effect as commit for replay: pending+rollback
//            pair drops out of the orphan set.
//
// DESIGN-self-host-state.md only names pending/commit (replay was
// only intended for hard crashes). rollback was added to preserve
// the "AddApp returning error implies no persistence" contract
// when soft failures happen after pending fsync — without it the
// next boot would materialise an intent the caller already
// observed as failed.
const (
	walTypePending  = "pending"
	walTypeCommit   = "commit"
	walTypeRollback = "rollback"
)

// walRecord is the NDJSON shape persisted in the WAL file. One JSON
// object per line.
//
// pending records carry the full post-mutation state.json bytes
// (base64) + sha256 of those bytes; the bytes are what replay
// re-writes on crash recovery. Carrying full bytes (not deltas) is
// deliberate for v0.0.x scale (≤500 apps × ~5KB per app metadata
// = ~2.5MB max per record). #4 (hash chain) may introduce
// compaction; for now the WAL grows unbounded — acceptable for
// Lawrence-dogfood scale.
//
// commit records carry only the token of the prior pending they
// confirm landed.
type walRecord struct {
	Type            string `json:"type"`
	Timestamp       string `json:"ts"`
	Token           string `json:"token"`
	StateHash       string `json:"state_hash,omitempty"`
	StatePayloadB64 string `json:"state_payload,omitempty"`
}

// newWALToken returns a fresh UUIDv7 used to pair pending+commit.
// Falls back to v4 on entropy failure (same fallback as
// newAppMetadata).
func newWALToken() string {
	u, err := uuid.NewV7()
	if err != nil {
		u = uuid.New()
	}
	return u.String()
}

// appendPending writes a pending record into walPath with the
// post-mutation state.json bytes + sha256 hash, then fsyncs the
// WAL file. Returns the token written so the caller can pair it
// with a subsequent commit.
func appendPending(path string, stateBytes []byte) (token string, err error) {
	hash := sha256.Sum256(stateBytes)
	token = newWALToken()
	rec := walRecord{
		Type:            walTypePending,
		Timestamp:       time.Now().UTC().Format(time.RFC3339Nano),
		Token:           token,
		StateHash:       "sha256:" + hex.EncodeToString(hash[:]),
		StatePayloadB64: base64.StdEncoding.EncodeToString(stateBytes),
	}
	return token, appendWAL(path, rec)
}

// appendCommit writes a commit record referencing token, then
// fsyncs the WAL file.
func appendCommit(path string, token string) error {
	return appendWAL(path, walRecord{
		Type:      walTypeCommit,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Token:     token,
	})
}

// appendRollback writes a rollback record referencing token, then
// fsyncs the WAL file. Used by flushFull's error path when a
// pending was written but a subsequent step failed (read-only
// directory, fsync EIO, etc) — the daemon wants the NEXT boot to
// treat pending+rollback as a no-op rather than replaying the
// intent the caller already observed as rejected.
func appendRollback(path string, token string) error {
	return appendWAL(path, walRecord{
		Type:      walTypeRollback,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Token:     token,
	})
}

// appendWAL is the low-level append-and-fsync primitive. Opens the
// file in O_APPEND mode so concurrent writers (which shouldn't exist
// under the flushMu contract) wouldn't corrupt each other anyway.
func appendWAL(path string, rec walRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("wal: marshal: %w", err)
	}
	line = append(line, '\n')

	// Detect first create so we can fsync the parent directory
	// afterwards — on ext4/xfs a newly-created file's directory entry
	// is not guaranteed durable until the directory itself is synced,
	// so without this the first pending record can be lost on crash.
	_, statErr := os.Stat(path)
	firstCreate := os.IsNotExist(statErr)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("wal: open %s: %w", path, err)
	}
	if err := writeAll(f, line); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: fsync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("wal: close: %w", err)
	}
	if firstCreate {
		if err := fsyncDir(filepath.Dir(path)); err != nil {
			return fmt.Errorf("wal: fsync dir after first create: %w", err)
		}
	}
	return nil
}

// scanOrphanPending reads the entire WAL file at path and returns
// pending records whose tokens have no matching commit, sorted by
// append order (which is also approximately timestamp order). On
// boot the caller replays the LATEST orphan — that record's
// payload reflects the most recent intended state.
//
// Missing file → empty result, no error (fresh install path).
func scanOrphanPending(path string) ([]walRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("wal: open for scan %s: %w", path, err)
	}
	defer f.Close()

	// pendings keyed by token + append-order index so we can
	// reconstruct order in the final result. Drop entries from the
	// map on commit/rollback so memory tracks orphan count, not total
	// WAL line count — important because each pending carries the
	// full base64'd state.json payload (multi-MB on busy hosts).
	type pendingWithOrder struct {
		rec   walRecord
		order int
	}
	pendings := make(map[string]pendingWithOrder)

	scanner := bufio.NewScanner(f)
	// Allow up to 16 MB per line (pending records carry full
	// state.json bytes base64-encoded; at 500 apps × ~5 KB that's
	// ~3 MB raw → ~4 MB base64 → some slack to spare).
	scanner.Buffer(make([]byte, 0, 4*1024*1024), 16*1024*1024)

	order := 0
	for scanner.Scan() {
		var rec walRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("wal: decode line %d in %s: %w", order, path, err)
		}
		order++
		switch rec.Type {
		case walTypePending:
			pendings[rec.Token] = pendingWithOrder{rec: rec, order: order}
		case walTypeCommit, walTypeRollback:
			// Both close the orphan tracking — commit means the
			// change landed, rollback means the daemon revoked it.
			// Drop the pending entry so its payload (potentially MB
			// of base64'd state) is GC-eligible immediately.
			delete(pendings, rec.Token)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("wal: scan %s: %w", path, err)
	}

	out := make([]pendingWithOrder, 0, len(pendings))
	for _, p := range pendings {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })

	recs := make([]walRecord, len(out))
	for i, p := range out {
		recs[i] = p.rec
	}
	return recs, nil
}

// decodePendingPayload extracts and verifies the state.json bytes
// from a pending record. Returns ErrWALHashMismatch if the embedded
// hash doesn't match the embedded payload (would indicate WAL file
// corruption).
func decodePendingPayload(rec walRecord) ([]byte, error) {
	if rec.Type != walTypePending {
		return nil, fmt.Errorf("wal: decodePendingPayload on non-pending record (type=%s)", rec.Type)
	}
	if rec.StatePayloadB64 == "" {
		return nil, fmt.Errorf("wal: pending record token=%s has empty payload", rec.Token)
	}
	data, err := base64.StdEncoding.DecodeString(rec.StatePayloadB64)
	if err != nil {
		return nil, fmt.Errorf("wal: pending base64 decode token=%s: %w", rec.Token, err)
	}
	wantHash := rec.StateHash
	gotHash := sha256.Sum256(data)
	got := "sha256:" + hex.EncodeToString(gotHash[:])
	if want := wantHash; got != want {
		return nil, &WALHashMismatchError{Token: rec.Token, Want: want, Got: got}
	}
	return data, nil
}

// WALHashMismatchError surfaces a corrupted WAL pending record —
// the embedded payload doesn't hash to the embedded hash. Indicates
// either WAL file corruption or tampering. Boot refuses to proceed
// per DESIGN-self-host-state.md §"Audit log as WAL — crash
// recovery".
type WALHashMismatchError struct {
	Token string
	Want  string
	Got   string
}

func (e *WALHashMismatchError) Error() string {
	return fmt.Sprintf("wal: pending record token=%s hash mismatch (want %s, got %s); "+
		"refusing to boot — investigate WAL file or restore from backup",
		e.Token, e.Want, e.Got)
}
