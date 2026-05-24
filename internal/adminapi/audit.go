package adminapi

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AuditEntry is one line in the audit log (NDJSON). PrevSHA256 is
// the sha256 of the FULL serialized bytes of the immediately
// preceding record (the JSON line without the trailing newline);
// genesis records use 64 hex zeros. The chain lets an off-host
// audit copy detect any tampering with the live log — if even one
// byte of a prior record changes, every subsequent record's
// PrevSHA256 will no longer match the recomputed sha256 of the
// tampered record.
//
// See DESIGN-self-host-state.md §"Audit log".
type AuditEntry struct {
	PrevSHA256 string `json:"prev_sha256"`
	Timestamp  string `json:"ts"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	AppID      string `json:"app_id,omitempty"`
	Action     string `json:"action"`
	Actor      string `json:"actor"`
	StatusCode int    `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	SourceIP   string `json:"source_ip"`
}

// auditGenesisHash is what the first-ever record uses for
// PrevSHA256. 64 hex zeros = 32-byte sha256-shaped value indicating
// "no prior record".
const auditGenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// auditMaxFileBytes is the rotation threshold per DESIGN — 25 MB
// per file, oldest 5 files kept on host = 125 MB on-host cap. v4's
// 100 MB × 5 was reduced to respect $5 VPS disk budgets.
const auditMaxFileBytes = 25 * 1024 * 1024

// auditKeepRotated is the number of rotated files retained on host.
// audit.log.1 (newest rotated) → audit.log.5 (oldest). All older
// content has been included in Tier 1 backup; on-host retention is
// for recent investigation only.
const auditKeepRotated = 5

// AuditLogger writes hash-chained audit entries to a file. Safe for
// concurrent use.
type AuditLogger struct {
	mu       sync.Mutex
	dir      string
	file     *os.File
	path     string
	bytes    int64  // current size of file in bytes (tracked for rotation)
	lastHash string // hex sha256 of the last record's serialized bytes
}

// NewAuditLogger creates an audit logger writing to the given
// directory. Creates the directory and file if they don't exist.
//
// Hash-chain boot continuity: if an existing audit.log is present
// from a prior process, the last line is parsed and re-hashed so
// the next Log call chains forward cleanly across process restart.
// On a fresh install the chain starts from the genesis hash.
//
// File permissions: 0640 (owner rw, group r). Dir: 0750.
func NewAuditLogger(dir string) (*AuditLogger, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "audit.log")
	a := &AuditLogger{
		dir:      dir,
		path:     path,
		lastHash: auditGenesisHash,
	}
	if err := a.resumeChain(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	a.file = f
	if st, err := f.Stat(); err == nil {
		a.bytes = st.Size()
	}
	return a, nil
}

// resumeChain reads the last line of an existing audit.log (if any)
// and computes its sha256 so the next Log call's PrevSHA256 points
// at the correct prior record. Without this, every process restart
// would emit a fresh genesis (chain break) — silent disaster for
// off-host integrity comparison.
//
// Empty/missing file → lastHash stays at the genesis sentinel.
func (a *AuditLogger) resumeChain() error {
	f, err := os.Open(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("audit: resume scan %s: %w", a.path, err)
	}
	defer f.Close()
	// Read whole file line-by-line; keep the last non-empty line.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lastLine []byte
	for scanner.Scan() {
		b := scanner.Bytes()
		if len(b) == 0 {
			continue
		}
		lastLine = append(lastLine[:0], b...)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("audit: resume scan: %w", err)
	}
	if len(lastLine) > 0 {
		h := sha256.Sum256(lastLine)
		a.lastHash = hex.EncodeToString(h[:])
	}
	return nil
}

// Log writes one audit entry. The chain link (PrevSHA256) is set
// from the in-memory lastHash; after the write succeeds, lastHash
// updates to sha256(this record's bytes). Encoding failures drop
// the entry silently — audit must not block the API path — but the
// chain semantic survives a dropped record because lastHash only
// advances when a write lands.
//
// File rotation fires when the post-write file size would exceed
// auditMaxFileBytes; see rotate().
func (a *AuditLogger) Log(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	e.PrevSHA256 = a.lastHash
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	if a.file != nil {
		n, werr := a.file.Write(append(line, '\n'))
		if werr != nil {
			return
		}
		a.bytes += int64(n)
	}
	h := sha256.Sum256(line)
	a.lastHash = hex.EncodeToString(h[:])

	// Check rotation AFTER the write so the record that pushed us
	// over the threshold lives in the file it was written to, and
	// the NEW file starts with the next record carrying
	// PrevSHA256 = sha256(this record) for cross-file chain
	// stitching.
	if a.bytes >= auditMaxFileBytes {
		_ = a.rotate()
	}
}

// rotate moves audit.log → audit.log.1, audit.log.1 → audit.log.2,
// etc up to audit.log.<auditKeepRotated>. The oldest rotation is
// discarded. Caller must hold a.mu.
//
// Chain stitching: the lastHash carried forward is the tip of the
// rotated-out file. The next Log call on the fresh audit.log will
// emit a record whose PrevSHA256 = that tip — making the chain
// traversable across rotation boundaries.
//
// audit-rotation-cursor file records the tip hash + the path of
// the rotated file so verification across boundaries can match
// up without re-reading every rotated file.
func (a *AuditLogger) rotate() error {
	if a.file != nil {
		if err := a.file.Close(); err != nil {
			return fmt.Errorf("audit: close before rotate: %w", err)
		}
		a.file = nil
	}
	// Shift .N → .N+1 from the highest backwards, dropping anything
	// past auditKeepRotated.
	for i := auditKeepRotated; i >= 1; i-- {
		src := a.rotatedPath(i)
		if i == auditKeepRotated {
			_ = os.Remove(src) // drop the oldest
			continue
		}
		dst := a.rotatedPath(i + 1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("audit: rotate %s → %s: %w", src, dst, err)
			}
		}
	}
	// Promote current audit.log → audit.log.1.
	if err := os.Rename(a.path, a.rotatedPath(1)); err != nil {
		return fmt.Errorf("audit: rotate %s → %s: %w", a.path, a.rotatedPath(1), err)
	}
	// Persist the rotation cursor for cross-file chain verification.
	if err := a.writeCursor(a.lastHash, a.rotatedPath(1)); err != nil {
		return fmt.Errorf("audit: write rotation cursor: %w", err)
	}
	// Open a fresh audit.log.
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return fmt.Errorf("audit: open new %s after rotate: %w", a.path, err)
	}
	a.file = f
	a.bytes = 0
	return nil
}

// rotatedPath returns audit.log.N for N ≥ 1.
func (a *AuditLogger) rotatedPath(n int) string {
	return fmt.Sprintf("%s.%d", a.path, n)
}

// cursorPath returns the rotation-cursor file path. The cursor is
// updated atomically at each rotate() to record the just-rotated
// file's tip hash.
func (a *AuditLogger) cursorPath() string {
	return filepath.Join(a.dir, "audit-rotation-cursor")
}

// auditRotationCursor is the persisted shape — one JSON object with
// the tip hash and the path of the file whose tip it is. Updated
// every rotation; verification uses it to stitch the chain across
// the current file and the most recent rotated file.
type auditRotationCursor struct {
	TipSHA256    string `json:"tip_sha256"`
	RotatedPath  string `json:"rotated_path"`
}

func (a *AuditLogger) writeCursor(tip, rotated string) error {
	cur := auditRotationCursor{TipSHA256: tip, RotatedPath: rotated}
	data, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	tmp := a.cursorPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0640); err != nil {
		return err
	}
	return os.Rename(tmp, a.cursorPath())
}

// Close flushes and closes the audit log file.
func (a *AuditLogger) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.file == nil {
		return nil
	}
	err := a.file.Close()
	a.file = nil
	return err
}

// VerifyAuditChain re-hashes every record in the audit.log at dir
// (and the most recent rotated file if a cursor is present) and
// confirms each record's PrevSHA256 matches the prior record's
// computed sha256. Returns nil when the chain is intact, or an
// AuditChainBrokenError pointing at the first divergence.
//
// Used by the restore drill (when implemented) and `creek host
// doctor --verify-audit` to detect tampering or accidental
// corruption.
func VerifyAuditChain(dir string) error {
	path := filepath.Join(dir, "audit.log")
	// First verify any rotated file (if cursor exists), then the
	// current file with the rotated file's tip as the boundary.
	cursorPath := filepath.Join(dir, "audit-rotation-cursor")
	expectedPrev := auditGenesisHash
	if data, err := os.ReadFile(cursorPath); err == nil {
		var cur auditRotationCursor
		if err := json.Unmarshal(data, &cur); err == nil && cur.RotatedPath != "" {
			tip, err := verifyChainInFile(cur.RotatedPath, auditGenesisHash)
			if err != nil {
				return err
			}
			if tip != cur.TipSHA256 {
				return &AuditChainBrokenError{
					File:    cur.RotatedPath,
					Reason:  fmt.Sprintf("computed tip %s does not match cursor's recorded tip %s", tip, cur.TipSHA256),
				}
			}
			expectedPrev = tip
		}
	}
	if _, err := verifyChainInFile(path, expectedPrev); err != nil {
		return err
	}
	return nil
}

// verifyChainInFile scans path and walks its records. Returns the
// final record's sha256 (the file's tip) so the caller can stitch
// across files. expectedPrev is what the first record's PrevSHA256
// must equal (genesis hash for a fresh chain, or the rotated file's
// tip for a continuation).
//
// Missing file → tip == expectedPrev, no error (the chain has not
// started yet).
func verifyChainInFile(path string, expectedPrev string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return expectedPrev, nil
		}
		return "", fmt.Errorf("audit verify: open %s: %w", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	prev := expectedPrev
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var rec AuditEntry
		if err := json.Unmarshal(raw, &rec); err != nil {
			return "", &AuditChainBrokenError{File: path, Line: lineNum, Reason: "decode: " + err.Error()}
		}
		if rec.PrevSHA256 != prev {
			return "", &AuditChainBrokenError{File: path, Line: lineNum,
				Reason: fmt.Sprintf("PrevSHA256=%s does not match prior record's computed sha256=%s", rec.PrevSHA256, prev)}
		}
		// Compute this record's sha256 over its full serialized
		// bytes (matching the bytes Log wrote, NOT the bytes the
		// scanner returned — the two should match since we use
		// json.Marshal both places, but be explicit).
		canonical, err := json.Marshal(rec)
		if err != nil {
			return "", fmt.Errorf("audit verify: re-marshal line %d: %w", lineNum, err)
		}
		h := sha256.Sum256(canonical)
		prev = hex.EncodeToString(h[:])
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("audit verify: scan %s: %w", path, err)
	}
	return prev, nil
}

// AuditChainBrokenError surfaces a chain break detected by
// VerifyAuditChain. File + Line locate the broken record; Reason
// names the specific failure (decode error, prev_sha256 mismatch,
// cursor tip mismatch).
type AuditChainBrokenError struct {
	File   string
	Line   int
	Reason string
}

func (e *AuditChainBrokenError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("audit: chain broken at %s:line %d — %s", e.File, e.Line, e.Reason)
	}
	return fmt.Sprintf("audit: chain broken at %s — %s", e.File, e.Reason)
}

// hashToken returns a short hash of the bearer token for audit
// identification without exposing the token itself.
func hashToken(token string) string {
	if token == "" {
		return "none"
	}
	h := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(h[:4])
}

// extractAppID pulls the app ID from the URL path if present.
// /v1/apps/my-app/deploy → "my-app"
//
// Returns "" when the URL is the collection root (/v1/apps or
// /v1/apps/). Importantly, it does NOT special-case the literal
// id "apps" — supervisor.ValidateID permits that name, and rejecting
// it here previously created a CAS bypass for any app the user
// happened to name "apps".
func extractAppID(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/apps/"), "/")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

// actionFromRequest derives a human-readable action from method + path.
func actionFromRequest(method, path string) string {
	switch {
	case method == "POST" && path == "/v1/apps":
		return "spawn"
	case method == "DELETE" && strings.HasPrefix(path, "/v1/apps/"):
		return "stop"
	case strings.HasSuffix(path, "/deploy"):
		return "deploy"
	case strings.HasSuffix(path, "/restart"):
		return "restart"
	case strings.HasSuffix(path, "/reset"):
		return "reset"
	case method == "POST" && strings.HasPrefix(path, "/v1/volumes"):
		return "volume_register"
	case method == "DELETE" && strings.HasPrefix(path, "/v1/volumes/"):
		return "volume_delete"
	default:
		return method + " " + path
	}
}

// isMutating returns true for methods that change state.
func isMutating(method string) bool {
	return method == "POST" || method == "DELETE" || method == "PUT" || method == "PATCH"
}

// auditResponseWriter wraps http.ResponseWriter to capture status code.
type auditResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *auditResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditResponseWriter) Write(b []byte) (int, error) {
	return w.ResponseWriter.Write(b)
}

// Unwrap supports http.Flusher passthrough for SSE endpoints.
func (w *auditResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// Flush implements http.Flusher by delegating to the wrapped writer.
func (w *auditResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// NopAuditLogger is a no-op audit logger for when auditing is disabled.
var NopAuditLogger io.Closer = nopCloser{}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
