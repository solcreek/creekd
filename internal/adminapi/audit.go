package adminapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AuditEntry is one line in the audit log (NDJSON).
type AuditEntry struct {
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

// AuditLogger writes structured audit entries to a file.
// Safe for concurrent use.
type AuditLogger struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// NewAuditLogger creates an audit logger writing to the given directory.
// Creates the directory and file if they don't exist.
// File permissions: 0640 (owner rw, group r). Dir: 0750.
func NewAuditLogger(dir string) (*AuditLogger, error) {
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("audit: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		return nil, fmt.Errorf("audit: open %s: %w", path, err)
	}
	return &AuditLogger{file: f, enc: json.NewEncoder(f)}, nil
}

// Log writes one audit entry. Non-blocking — if encoding fails, the
// entry is silently dropped (audit should not break the API).
func (a *AuditLogger) Log(e AuditEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	_ = a.enc.Encode(e)
}

// Close flushes and closes the audit log file.
func (a *AuditLogger) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.file.Close()
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
