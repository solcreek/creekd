package logs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixedTime returns a deterministic NowFunc anchored to ts and
// advancing by 1 ms per call. Useful for assertions on ordering.
func fixedTime(ts time.Time) func() time.Time {
	var i int64
	var mu sync.Mutex
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t := ts.Add(time.Duration(i) * time.Millisecond)
		i++
		return t
	}
}

func readLines(t *testing.T, path string) []logRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := make([]logRecord, 0, len(lines))
	for _, ln := range lines {
		var r logRecord
		if err := json.Unmarshal([]byte(ln), &r); err != nil {
			t.Fatalf("decode %q: %v", ln, err)
		}
		out = append(out, r)
	}
	return out
}

func TestRotatorWritesJSONLine(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "myapp", Options{
		NowFunc: fixedTime(time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)),
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	w := r.Stdout()
	if _, err := w.Write([]byte("hello world\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	recs := readLines(t, filepath.Join(dir, "myapp", "current.log"))
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].App != "myapp" {
		t.Errorf("app = %q, want myapp", recs[0].App)
	}
	if recs[0].Stream != "stdout" {
		t.Errorf("stream = %q, want stdout", recs[0].Stream)
	}
	if recs[0].Msg != "hello world" {
		t.Errorf("msg = %q, want %q", recs[0].Msg, "hello world")
	}
	if recs[0].Ts == "" {
		t.Errorf("ts missing")
	}
}

func TestRotatorMultipleLinesInOneWrite(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "multi", Options{
		NowFunc: fixedTime(time.Now()),
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	w := r.Stdout()
	_, _ = w.Write([]byte("line1\nline2\nline3\n"))

	recs := readLines(t, filepath.Join(dir, "multi", "current.log"))
	if len(recs) != 3 {
		t.Fatalf("want 3 records, got %d", len(recs))
	}
	for i, want := range []string{"line1", "line2", "line3"} {
		if recs[i].Msg != want {
			t.Errorf("recs[%d].Msg = %q, want %q", i, recs[i].Msg, want)
		}
	}
}

func TestRotatorBuffersPartialLine(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "partial", Options{NowFunc: fixedTime(time.Now())})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	w := r.Stdout()
	_, _ = w.Write([]byte("abc"))
	// Nothing flushed yet.
	if recs := readLines(t, filepath.Join(dir, "partial", "current.log")); len(recs) != 0 {
		t.Errorf("partial flushed prematurely: %d records", len(recs))
	}
	_, _ = w.Write([]byte("def\nghi"))
	recs := readLines(t, filepath.Join(dir, "partial", "current.log"))
	if len(recs) != 1 || recs[0].Msg != "abcdef" {
		t.Errorf("after second write: %+v", recs)
	}

	// Close flushes the tail.
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recs = readLines(t, filepath.Join(dir, "partial", "current.log"))
	if len(recs) != 2 || recs[1].Msg != "ghi" {
		t.Errorf("after Close: %+v", recs)
	}
}

func TestRotatorRotatesAtThreshold(t *testing.T) {
	dir := t.TempDir()
	// Tiny threshold so a few short lines trigger rotation.
	r, err := NewRotator(dir, "rot", Options{
		MaxSizeBytes: 200,
		MaxBackups:   3,
		NowFunc:      fixedTime(time.Now()),
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	w := r.Stdout()
	// Each JSON record is ~80 bytes; write enough to force ≥1 rotation.
	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte("xxxxxxxxxx\n")); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}

	currentPath := filepath.Join(dir, "rot", "current.log")
	if info, err := os.Stat(currentPath); err == nil {
		if info.Size() > 200+200 { // generous slack for the in-flight line
			t.Errorf("current.log size %d exceeds threshold; rotation didn't happen", info.Size())
		}
	}

	// At least one backup must exist.
	if _, err := os.Stat(filepath.Join(dir, "rot", "current.log.1")); err != nil {
		t.Errorf("current.log.1 missing: %v", err)
	}
}

func TestRotatorEnforcesMaxBackups(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "many", Options{
		MaxSizeBytes: 100,
		MaxBackups:   2,
		NowFunc:      fixedTime(time.Now()),
	})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	w := r.Stdout()
	// Force ≥4 rotations.
	for i := 0; i < 80; i++ {
		_, _ = w.Write([]byte("zzzzzzzzzz\n"))
	}

	// .1 and .2 must exist; .3 must not.
	mustExist := []string{"current.log.1", "current.log.2"}
	mustNotExist := []string{"current.log.3"}
	for _, name := range mustExist {
		if _, err := os.Stat(filepath.Join(dir, "many", name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	for _, name := range mustNotExist {
		if _, err := os.Stat(filepath.Join(dir, "many", name)); err == nil {
			t.Errorf("%s exists; MaxBackups=2 not enforced", name)
		}
	}
}

func TestRotatorStdoutStderrInterleave(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "mix", Options{NowFunc: fixedTime(time.Now())})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	defer r.Close()

	// Two goroutines write concurrently; the rotator's mutex must
	// keep records intact (no torn JSON).
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		w := r.Stdout()
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte("out\n"))
		}
	}()
	go func() {
		defer wg.Done()
		w := r.Stderr()
		for i := 0; i < 50; i++ {
			_, _ = w.Write([]byte("err\n"))
		}
	}()
	wg.Wait()

	recs := readLines(t, filepath.Join(dir, "mix", "current.log"))
	if len(recs) != 100 {
		t.Errorf("want 100 records, got %d", len(recs))
	}
	var outs, errs int
	for _, rec := range recs {
		switch rec.Stream {
		case "stdout":
			outs++
		case "stderr":
			errs++
		default:
			t.Errorf("unknown stream %q", rec.Stream)
		}
	}
	if outs != 50 || errs != 50 {
		t.Errorf("stream counts: stdout=%d stderr=%d, want 50/50", outs, errs)
	}
}

func TestRotatorReopensExistingFile(t *testing.T) {
	dir := t.TempDir()

	// First rotator writes one line, closes.
	r1, err := NewRotator(dir, "persist", Options{NowFunc: fixedTime(time.Now())})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	_, _ = r1.Stdout().Write([]byte("first run\n"))
	_ = r1.Close()

	// Second rotator opens the same dir; existing line must still be present.
	r2, err := NewRotator(dir, "persist", Options{NowFunc: fixedTime(time.Now())})
	if err != nil {
		t.Fatalf("NewRotator (reopen): %v", err)
	}
	defer r2.Close()
	_, _ = r2.Stdout().Write([]byte("second run\n"))

	recs := readLines(t, filepath.Join(dir, "persist", "current.log"))
	if len(recs) != 2 {
		t.Fatalf("want 2 records (first + second run), got %d", len(recs))
	}
	if recs[0].Msg != "first run" || recs[1].Msg != "second run" {
		t.Errorf("records out of order: %+v", recs)
	}
}

func TestRotatorRejectsEmptyDirOrApp(t *testing.T) {
	if _, err := NewRotator("", "app", Options{}); err == nil {
		t.Error("expected error for empty dir")
	}
	if _, err := NewRotator(t.TempDir(), "", Options{}); err == nil {
		t.Error("expected error for empty appID")
	}
}

func TestRotatorWriteAfterCloseFails(t *testing.T) {
	dir := t.TempDir()
	r, err := NewRotator(dir, "closed", Options{NowFunc: fixedTime(time.Now())})
	if err != nil {
		t.Fatalf("NewRotator: %v", err)
	}
	w := r.Stdout()
	_, _ = w.Write([]byte("before\n"))
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write([]byte("after\n")); err == nil {
		t.Error("expected error writing after Close")
	}
}

func TestStreamWriterCloseEmptyBufferIsNoop(t *testing.T) {
	dir := t.TempDir()
	r, _ := NewRotator(dir, "noop", Options{NowFunc: fixedTime(time.Now())})
	defer r.Close()
	w := r.Stdout()
	if err := w.Close(); err != nil {
		t.Errorf("Close on empty buffer: %v", err)
	}
}
