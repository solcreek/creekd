package logs

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Logs benchmarks measure the per-line cost (JSON marshal + lock +
// fsync-free append) of the rotator's hot path, plus the cliff
// induced by rotation. b.SetBytes lets `go test -bench` print MB/s.
//
// Run: go test -bench=. -benchmem -run=^$ ./internal/logs/

// BenchmarkRotatorWriteShortLine measures the common app log shape
// (~50 chars per line).
func BenchmarkRotatorWriteShortLine(b *testing.B) {
	r := newBenchRotator(b, 0)
	defer r.Close()
	w := r.Stdout()
	line := []byte("listening on port 8080\n")

	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRotatorWriteLongLine targets the wide-line case (request
// log with a long URL, 400 chars). Marshalling cost dominates here.
func BenchmarkRotatorWriteLongLine(b *testing.B) {
	r := newBenchRotator(b, 0)
	defer r.Close()
	w := r.Stdout()
	body := strings.Repeat("x", 400) + "\n"
	line := []byte(body)

	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRotatorWriteBatched batches many lines into one Write —
// the io.Copy from a child's stdout pipe will hand the rotator
// kilobytes at a time, not one line per call.
func BenchmarkRotatorWriteBatched(b *testing.B) {
	r := newBenchRotator(b, 0)
	defer r.Close()
	w := r.Stdout()

	var buf bytes.Buffer
	for i := 0; i < 32; i++ {
		buf.WriteString("listening on port 8080\n")
	}
	chunk := buf.Bytes()

	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Write(chunk); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRotatorUnderRotation forces a small MaxSize so every few
// iterations triggers a rotation (rename + reopen). Measures the
// amortised cost when an app is logging fast enough to roll the
// file regularly.
func BenchmarkRotatorUnderRotation(b *testing.B) {
	r := newBenchRotator(b, 4*1024) // tiny 4 KiB threshold
	defer r.Close()
	w := r.Stdout()
	line := []byte("listening on port 8080\n")

	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Write(line); err != nil {
			b.Fatal(err)
		}
	}
}

// newBenchRotator wires a Rotator at b.TempDir() with a fixed
// timestamp (so JSON output is deterministic per iteration and we
// don't measure time.Now overhead).
func newBenchRotator(b *testing.B, maxSize int64) *Rotator {
	b.Helper()
	fixed := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r, err := NewRotator(b.TempDir(), "bench", Options{
		MaxSizeBytes: maxSize, // 0 → DefaultMaxSize
		MaxBackups:   3,
		NowFunc:      func() time.Time { return fixed },
	})
	if err != nil {
		b.Fatalf("NewRotator: %v", err)
	}
	return r
}
