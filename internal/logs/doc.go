// Package logs captures stdout/stderr from child processes, wraps each
// line in structured JSON, and writes to per-app log files with
// rotation.
//
// M5.6 — log capture + rotation + structured JSON
//
// Output format (one JSON object per line):
//
//	{"ts":"2026-06-01T10:00:00Z","app":"my-app","stream":"stdout","msg":"..."}
//
// Files: /var/log/creekd/{appID}/current.log
// Rotation: 10 MB max per file, 5 history files retained.
//
// Recommended library: gopkg.in/natefinch/lumberjack.v2 for rotation;
// zap or slog for structured logging.
package logs
