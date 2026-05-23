package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DefaultMaxSize is the default rotation threshold (10 MiB).
const DefaultMaxSize int64 = 10 * 1024 * 1024

// DefaultMaxBackups is the number of rotated files retained on top of
// the current one (current.log + current.log.1 .. current.log.5).
const DefaultMaxBackups = 5

// Options tunes a Rotator. Zero-valued fields use defaults.
type Options struct {
	MaxSizeBytes int64
	MaxBackups   int
	// NowFunc supplies the timestamp recorded in each line. Useful for
	// deterministic tests; defaults to time.Now.
	NowFunc func() time.Time
}

// Rotator owns one /var/log/creekd/<app>/current.log file. It serialises
// concurrent writes from the stdout and stderr StreamWriters of the same
// app, wraps each line as JSON, and rotates the file once it exceeds
// MaxSizeBytes.
//
// One Rotator per app. Two StreamWriters per app (stdout, stderr) sit
// in front of it.
type Rotator struct {
	dir   string
	appID string
	opts  Options

	mu   sync.Mutex
	file *os.File
	size int64

	// Long-lived StreamWriters so any partial trailing line buffered
	// from a previous child invocation is flushed when the rotator
	// closes. Stdout() and Stderr() return these instances.
	stdoutW *StreamWriter
	stderrW *StreamWriter
}

// NewRotator creates a Rotator for one app. The directory dir/<appID>
// is created if it does not exist. current.log is opened for append.
func NewRotator(dir, appID string, opts Options) (*Rotator, error) {
	if appID == "" {
		return nil, errors.New("logs: empty appID")
	}
	if dir == "" {
		return nil, errors.New("logs: empty dir")
	}
	if opts.MaxSizeBytes <= 0 {
		opts.MaxSizeBytes = DefaultMaxSize
	}
	if opts.MaxBackups <= 0 {
		opts.MaxBackups = DefaultMaxBackups
	}
	if opts.NowFunc == nil {
		opts.NowFunc = time.Now
	}

	appDir := filepath.Join(dir, appID)
	if err := os.MkdirAll(appDir, 0o750); err != nil {
		return nil, fmt.Errorf("logs: mkdir %s: %w", appDir, err)
	}

	r := &Rotator{dir: dir, appID: appID, opts: opts}
	r.stdoutW = &StreamWriter{r: r, stream: "stdout"}
	r.stderrW = &StreamWriter{r: r, stream: "stderr"}
	if err := r.openCurrent(); err != nil {
		return nil, err
	}
	return r, nil
}

// Close flushes any partial trailing lines from both StreamWriters and
// then closes the underlying file. Idempotent.
func (r *Rotator) Close() error {
	// Flush trailing partial lines before we close the file. These
	// calls acquire r.mu internally via writeLine.
	_ = r.stdoutW.Close()
	_ = r.stderrW.Close()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

// Stdout returns the rotator's long-lived stdout writer. The same
// instance is returned on every call; closing the rotator flushes any
// buffered partial line.
func (r *Rotator) Stdout() *StreamWriter { return r.stdoutW }

// Stderr returns the rotator's long-lived stderr writer.
func (r *Rotator) Stderr() *StreamWriter { return r.stderrW }

// currentPath returns the path to the active log file.
func (r *Rotator) currentPath() string {
	return filepath.Join(r.dir, r.appID, "current.log")
}

// backupPath returns the path for backup index n (1-based).
func (r *Rotator) backupPath(n int) string {
	return filepath.Join(r.dir, r.appID, fmt.Sprintf("current.log.%d", n))
}

// openCurrent (re)opens current.log for append and records its size.
// Caller must hold r.mu OR be inside a constructor.
func (r *Rotator) openCurrent() error {
	path := r.currentPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("logs: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("logs: stat %s: %w", path, err)
	}
	r.file = f
	r.size = info.Size()
	return nil
}

// writeLine emits one record. msg is the raw line text (without trailing
// newline). Rotates before writing if the projected size would exceed
// the limit.
func (r *Rotator) writeLine(stream, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return errors.New("logs: writer closed")
	}

	rec := logRecord{
		Ts:     r.opts.NowFunc().UTC().Format(time.RFC3339Nano),
		App:    r.appID,
		Stream: stream,
		Msg:    msg,
	}
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("logs: marshal: %w", err)
	}
	buf = append(buf, '\n')

	if r.size+int64(len(buf)) > r.opts.MaxSizeBytes {
		if err := r.rotateLocked(); err != nil {
			return err
		}
	}

	n, err := r.file.Write(buf)
	r.size += int64(n)
	return err
}

// rotateLocked closes the current file, shifts backups (.4 → .5,
// .3 → .4, …, .1 → .2, current.log → .1), drops .6, and opens a fresh
// current.log. Caller holds r.mu.
func (r *Rotator) rotateLocked() error {
	if r.file != nil {
		if err := r.file.Close(); err != nil {
			return fmt.Errorf("logs: close before rotate: %w", err)
		}
		r.file = nil
	}

	// Drop the oldest if it would push past MaxBackups.
	oldest := r.backupPath(r.opts.MaxBackups)
	_ = os.Remove(oldest) // best-effort; ignore not-exist

	// Shift backups upward: .(N-1) → .N, .(N-2) → .(N-1), … .1 → .2
	for i := r.opts.MaxBackups - 1; i >= 1; i-- {
		from := r.backupPath(i)
		to := r.backupPath(i + 1)
		if _, err := os.Stat(from); err != nil {
			continue // missing — nothing to shift
		}
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("logs: rename %s → %s: %w", from, to, err)
		}
	}

	// current.log → .1
	if _, err := os.Stat(r.currentPath()); err == nil {
		if err := os.Rename(r.currentPath(), r.backupPath(1)); err != nil {
			return fmt.Errorf("logs: rename current → .1: %w", err)
		}
	}

	return r.openCurrent()
}

// logRecord is the JSON line layout. Field order is deterministic via
// struct tag ordering in encoding/json.
type logRecord struct {
	Ts     string `json:"ts"`
	App    string `json:"app"`
	Stream string `json:"stream"`
	Msg    string `json:"msg"`
}

// StreamWriter is an io.Writer adapter that splits its input on \n and
// hands each complete line to its Rotator. The trailing partial line is
// buffered until the next Write or Close. Each StreamWriter is owned by
// one goroutine (the io.Copy loop from the corresponding pipe).
type StreamWriter struct {
	r      *Rotator
	stream string
	buf    bytes.Buffer
}

// Write implements io.Writer.
func (s *StreamWriter) Write(p []byte) (int, error) {
	s.buf.Write(p)
	for {
		data := s.buf.Bytes()
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := string(data[:i])
		// Drop the line + the newline.
		s.buf.Next(i + 1)
		if err := s.r.writeLine(s.stream, line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// Close flushes any buffered partial line as a final record. Safe to
// call multiple times.
func (s *StreamWriter) Close() error {
	if s.buf.Len() == 0 {
		return nil
	}
	line := s.buf.String()
	s.buf.Reset()
	return s.r.writeLine(s.stream, line)
}

// Ensure StreamWriter satisfies the standard interfaces.
var (
	_ io.Writer = (*StreamWriter)(nil)
	_ io.Closer = (*StreamWriter)(nil)
)
