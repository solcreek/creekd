package adminapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditLoggerWritesEntry(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer logger.Close()

	logger.Log(AuditEntry{
		Timestamp:  "2026-05-22T12:00:00Z",
		Method:     "POST",
		Path:       "/v1/apps",
		AppID:      "",
		Action:     "spawn",
		Actor:      "sha256:abcd1234",
		StatusCode: 200,
		DurationMS: 45,
		SourceIP:   "127.0.0.1:54321",
	})

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}

	var entry AuditEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("parse entry: %v\nraw: %s", err, data)
	}
	if entry.Action != "spawn" {
		t.Errorf("action = %q, want spawn", entry.Action)
	}
	if entry.StatusCode != 200 {
		t.Errorf("status = %d, want 200", entry.StatusCode)
	}
}

func TestAuditLoggerDirPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer logger.Close()

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0750 {
		t.Errorf("dir perm = %o, want 0750", perm)
	}

	logFi, err := os.Stat(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	if perm := logFi.Mode().Perm(); perm != 0640 {
		t.Errorf("log perm = %o, want 0640", perm)
	}
}

func TestHashToken(t *testing.T) {
	h1 := hashToken("secret-token-123")
	if !strings.HasPrefix(h1, "sha256:") {
		t.Errorf("hash = %q, want sha256: prefix", h1)
	}
	if len(h1) != len("sha256:")+8 {
		t.Errorf("hash length = %d, want %d", len(h1), len("sha256:")+8)
	}

	// Same token → same hash
	h2 := hashToken("secret-token-123")
	if h1 != h2 {
		t.Error("same token should produce same hash")
	}

	// Different token → different hash
	h3 := hashToken("different-token")
	if h1 == h3 {
		t.Error("different tokens should produce different hashes")
	}

	// Empty token
	if hashToken("") != "none" {
		t.Errorf("empty token hash = %q, want 'none'", hashToken(""))
	}
}

func TestExtractAppID(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/v1/apps", ""},
		{"/v1/apps/", ""},
		{"/v1/apps/my-app", "my-app"},
		{"/v1/apps/my-app/deploy", "my-app"},
		{"/v1/apps/my-app/restart", "my-app"},
		{"/v1/apps/my-app/logs", "my-app"},
		// Regression: an app legitimately named "apps" must still
		// extract as "apps" — supervisor.ValidateID permits the name,
		// and a CAS-eligible endpoint on it must not bypass the
		// middleware just because the id collides with the collection
		// segment.
		{"/v1/apps/apps", "apps"},
		{"/v1/apps/apps/deploy", "apps"},
		{"/v1/volumes", ""},
		{"/v1/volumes/vol-1", ""},
	}
	for _, c := range cases {
		got := extractAppID(c.path)
		if got != c.want {
			t.Errorf("extractAppID(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestActionFromRequest(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/v1/apps", "spawn"},
		{"DELETE", "/v1/apps/my-app", "stop"},
		{"POST", "/v1/apps/my-app/deploy", "deploy"},
		{"POST", "/v1/apps/my-app/restart", "restart"},
		{"POST", "/v1/apps/my-app/reset", "reset"},
	}
	for _, c := range cases {
		got := actionFromRequest(c.method, c.path)
		if got != c.want {
			t.Errorf("actionFromRequest(%s, %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestIsMutating(t *testing.T) {
	if !isMutating("POST") {
		t.Error("POST should be mutating")
	}
	if !isMutating("DELETE") {
		t.Error("DELETE should be mutating")
	}
	if isMutating("GET") {
		t.Error("GET should not be mutating")
	}
}

func TestAuditMultipleEntries(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}

	for i := 0; i < 5; i++ {
		logger.Log(AuditEntry{Action: "spawn", StatusCode: 200})
	}
	logger.Close()

	data, err := os.ReadFile(filepath.Join(dir, "audit.log"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Errorf("lines = %d, want 5", len(lines))
	}
}

// --- #4 hash chain tests ------------------------------------------

// TestAuditChain_GenesisRecordZeroPrev covers the first-record
// invariant: PrevSHA256 must be 64 hex zeros (the genesis sentinel).
func TestAuditChain_GenesisRecordZeroPrev(t *testing.T) {
	dir := t.TempDir()
	logger, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("NewAuditLogger: %v", err)
	}
	defer logger.Close()
	logger.Log(AuditEntry{Timestamp: "t1", Action: "spawn"})

	data, _ := os.ReadFile(filepath.Join(dir, "audit.log"))
	var first AuditEntry
	if err := json.Unmarshal(data, &first); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if first.PrevSHA256 != auditGenesisHash {
		t.Errorf("genesis PrevSHA256 = %q, want %q", first.PrevSHA256, auditGenesisHash)
	}
}

// TestAuditChain_SecondRecordChainsToFirst proves the inductive
// step: record N+1's PrevSHA256 equals sha256(serialized bytes of
// record N).
func TestAuditChain_SecondRecordChainsToFirst(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewAuditLogger(dir)
	defer logger.Close()
	logger.Log(AuditEntry{Timestamp: "t1", Action: "spawn"})
	logger.Log(AuditEntry{Timestamp: "t2", Action: "deploy"})

	data, _ := os.ReadFile(filepath.Join(dir, "audit.log"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 records, got %d", len(lines))
	}
	firstBytes := []byte(lines[0])
	wantPrev := hex.EncodeToString(sha256Sum(firstBytes))

	var second AuditEntry
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("decode second: %v", err)
	}
	if second.PrevSHA256 != wantPrev {
		t.Errorf("second.PrevSHA256 = %q, want %q (sha256 of first record's bytes)",
			second.PrevSHA256, wantPrev)
	}
}

// TestAuditChain_BootContinuityResumesChain covers the
// process-restart path: reopening an existing audit.log must NOT
// reset to genesis. The next record after restart must chain back
// to the LAST record from the previous lifecycle.
func TestAuditChain_BootContinuityResumesChain(t *testing.T) {
	dir := t.TempDir()
	l1, _ := NewAuditLogger(dir)
	l1.Log(AuditEntry{Timestamp: "t1", Action: "spawn"})
	l1.Log(AuditEntry{Timestamp: "t2", Action: "deploy"})
	_ = l1.Close()

	// Process restart — fresh logger over the same dir.
	l2, err := NewAuditLogger(dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer l2.Close()
	l2.Log(AuditEntry{Timestamp: "t3", Action: "rollback"})

	data, _ := os.ReadFile(filepath.Join(dir, "audit.log"))
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records (2 + 1 post-restart), got %d", len(lines))
	}
	secondBytes := []byte(lines[1])
	wantPrev := hex.EncodeToString(sha256Sum(secondBytes))

	var third AuditEntry
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatalf("decode third: %v", err)
	}
	if third.PrevSHA256 != wantPrev {
		t.Errorf("post-restart record PrevSHA256 = %q, want %q (chain broken across restart)",
			third.PrevSHA256, wantPrev)
	}
}

// TestAuditChain_VerifyIntactChain covers the happy path for the
// VerifyAuditChain helper.
func TestAuditChain_VerifyIntactChain(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewAuditLogger(dir)
	for i := 0; i < 5; i++ {
		logger.Log(AuditEntry{Timestamp: fmt.Sprintf("t%d", i), Action: "ping"})
	}
	_ = logger.Close()
	if err := VerifyAuditChain(dir); err != nil {
		t.Errorf("VerifyAuditChain on intact log: %v", err)
	}
}

// TestAuditChain_VerifyDetectsTamper covers the chain-break
// detection. Mutate a record's body on disk; subsequent record's
// PrevSHA256 no longer matches the recomputed sha256 of the
// tampered bytes → VerifyAuditChain returns AuditChainBrokenError.
func TestAuditChain_VerifyDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewAuditLogger(dir)
	logger.Log(AuditEntry{Timestamp: "t1", Action: "spawn"})
	logger.Log(AuditEntry{Timestamp: "t2", Action: "deploy"})
	logger.Log(AuditEntry{Timestamp: "t3", Action: "rollback"})
	_ = logger.Close()

	path := filepath.Join(dir, "audit.log")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	// Tamper line 1 (the second record) by swapping its action.
	var second AuditEntry
	_ = json.Unmarshal([]byte(lines[1]), &second)
	second.Action = "tampered"
	tampered, _ := json.Marshal(second)
	lines[1] = string(tampered)
	rewritten := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatalf("re-write tampered log: %v", err)
	}

	err := VerifyAuditChain(dir)
	var cb *AuditChainBrokenError
	if !errors.As(err, &cb) {
		t.Fatalf("VerifyAuditChain returned %v, want AuditChainBrokenError", err)
	}
	// Detection should fire on line 3 (record N+1 whose PrevSHA256
	// references the now-tampered record N).
	if cb.Line != 3 {
		t.Errorf("chain break detected at line %d, want 3 (the next record after tampered)", cb.Line)
	}
}

// TestAuditChain_RotationStitchesChain proves that across a
// rotation boundary the chain still verifies — the cursor records
// the rotated file's tip, and the new audit.log's first record's
// PrevSHA256 picks up there.
func TestAuditChain_RotationStitchesChain(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewAuditLogger(dir)

	// Hand-fire a rotation: write one record, then manually call
	// rotate() — much cheaper than writing 25 MB of records.
	logger.Log(AuditEntry{Timestamp: "before-rot", Action: "spawn"})
	logger.mu.Lock()
	if err := logger.rotate(); err != nil {
		logger.mu.Unlock()
		t.Fatalf("manual rotate: %v", err)
	}
	logger.mu.Unlock()
	logger.Log(AuditEntry{Timestamp: "after-rot", Action: "deploy"})
	_ = logger.Close()

	// Verify across the rotation boundary.
	if err := VerifyAuditChain(dir); err != nil {
		t.Errorf("VerifyAuditChain across rotation boundary: %v", err)
	}

	// Spot-check: new audit.log's first record's PrevSHA256 ==
	// rotated file's last-line sha256.
	rotated, _ := os.ReadFile(filepath.Join(dir, "audit.log.1"))
	rotatedLines := strings.Split(strings.TrimSpace(string(rotated)), "\n")
	wantPrev := hex.EncodeToString(sha256Sum([]byte(rotatedLines[len(rotatedLines)-1])))

	current, _ := os.ReadFile(filepath.Join(dir, "audit.log"))
	var first AuditEntry
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(string(current)), "\n")[0]), &first); err != nil {
		t.Fatalf("decode post-rotation first: %v", err)
	}
	if first.PrevSHA256 != wantPrev {
		t.Errorf("post-rotation first record PrevSHA256 = %q, want %q (rotated file's tip)",
			first.PrevSHA256, wantPrev)
	}
}

// TestAuditChain_RotationCursorMismatch covers the corruption
// detection on the rotation-cursor file itself: if someone tampers
// with the rotated file after rotation, the cursor's recorded tip
// no longer matches the recomputed tip of the rotated file →
// AuditChainBrokenError.
func TestAuditChain_RotationCursorMismatch(t *testing.T) {
	dir := t.TempDir()
	logger, _ := NewAuditLogger(dir)
	logger.Log(AuditEntry{Timestamp: "t1", Action: "spawn"})
	logger.mu.Lock()
	_ = logger.rotate()
	logger.mu.Unlock()
	logger.Log(AuditEntry{Timestamp: "t2", Action: "deploy"})
	_ = logger.Close()

	// Tamper with the rotated file: change one byte. The cursor's
	// recorded tip will no longer match the recomputed tip.
	rotPath := filepath.Join(dir, "audit.log.1")
	raw, _ := os.ReadFile(rotPath)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	var rec AuditEntry
	_ = json.Unmarshal([]byte(lines[0]), &rec)
	rec.Action = "tampered"
	tamp, _ := json.Marshal(rec)
	lines[0] = string(tamp)
	_ = os.WriteFile(rotPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600)

	err := VerifyAuditChain(dir)
	var cb *AuditChainBrokenError
	if !errors.As(err, &cb) {
		t.Errorf("VerifyAuditChain after rotated-file tamper returned %v, want AuditChainBrokenError", err)
	}
}

// sha256Sum is a test helper — keeps the test code free of
// crypto/sha256 array→slice plumbing at every call site.
func sha256Sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}
