package adminapi

import (
	"encoding/json"
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
		{"/v1/apps/my-app", "my-app"},
		{"/v1/apps/my-app/deploy", "my-app"},
		{"/v1/apps/my-app/restart", "my-app"},
		{"/v1/apps/my-app/logs", "my-app"},
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
