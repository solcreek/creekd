package adminapi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// defaultTail is the number of lines returned when ?tail is not set.
const defaultTail = 100

// maxTail caps user-requested tail size so a hostile client can't ask
// us to read an unbounded slab of memory.
const maxTail = 10_000

// followPollInterval is how often the streaming follower checks the
// log file for new content.
var followPollInterval = 500 * time.Millisecond

// handleLogs implements GET /v1/apps/{id}/logs?tail=N&follow=true.
//
// tail=N    return the last N lines as text/plain (default 100, max
//
//	10000). Each line is one JSON record from the rotator.
//
// follow=1  stream new lines as Server-Sent Events. The initial
//
//	response includes the tail; subsequent events arrive as
//	the log grows. Rotation is detected by file-size shrink
//	and triggers a re-read from offset 0.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if app := s.sup.Get(id); app == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "app not found")
		return
	}

	path := s.sup.AppLogPath(id)
	if path == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest,
			"log capture disabled (set Supervisor.LogDir)")
		return
	}

	tail, err := parseTail(r.URL.Query().Get("tail"))
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	follow := r.URL.Query().Get("follow") == "true"

	if follow {
		streamLogs(w, r, path, tail)
		return
	}
	tailLogs(w, path, tail)
}

// parseTail returns the validated tail size or an error.
func parseTail(raw string) (int, error) {
	if raw == "" {
		return defaultTail, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("tail: %w", err)
	}
	if n < 0 {
		return 0, errors.New("tail: must be >= 0")
	}
	if n > maxTail {
		return 0, fmt.Errorf("tail: must be <= %d", maxTail)
	}
	return n, nil
}

// tailLogs writes the last n lines of path as plain text. Missing
// files are returned as empty 200 — the app may simply not have
// produced output yet.
func tailLogs(w http.ResponseWriter, path string, n int) {
	lines, err := readTail(path, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, l := range lines {
		_, _ = w.Write([]byte(l))
		_, _ = w.Write([]byte{'\n'})
	}
}

// readTail returns the last n lines of path. A missing file yields
// an empty slice with no error. n == 0 returns no lines.
func readTail(path string, n int) ([]string, error) {
	if n == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer f.Close()

	// Phase 1: read whole file, split, slice. Files are rotated at
	// 10 MiB so worst-case memory is bounded.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	all := make([]string, 0, n)
	for scanner.Scan() {
		all = append(all, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	if len(all) <= n {
		return all, nil
	}
	return all[len(all)-n:], nil
}

// streamLogs writes the tail as SSE events, then polls the file for
// new lines and streams them until the client disconnects. Rotation
// (size shrink) resets the read position to 0.
func streamLogs(w http.ResponseWriter, r *http.Request, path string, initialTail int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"streaming not supported by underlying writer")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Initial tail.
	initial, err := readTail(path, initialTail)
	if err != nil {
		sendSSEEvent(w, flusher, "error", err.Error())
		return
	}
	for _, line := range initial {
		sendSSEData(w, flusher, line)
	}

	// Track byte position. Start at current end of file so the follow
	// loop sees only NEW writes (the tail already covered everything
	// up to here).
	var pos int64
	if info, err := os.Stat(path); err == nil {
		pos = info.Size()
	}

	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}

		info, err := os.Stat(path)
		if err != nil {
			// File temporarily missing during rotation. Try again next tick.
			continue
		}
		size := info.Size()
		if size < pos {
			// File shrank — rotation happened. Restart from 0.
			pos = 0
		}
		if size == pos {
			continue
		}

		appended, newPos, err := readFromOffset(path, pos)
		if err != nil {
			continue // transient — try again next tick
		}
		for _, line := range appended {
			sendSSEData(w, flusher, line)
		}
		pos = newPos
	}
}

// readFromOffset returns all complete lines starting at offset, plus
// the new offset (== file size after reading).
func readFromOffset(path string, offset int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var out []string
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	end, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return out, offset, err
	}
	return out, end, scanner.Err()
}

// sendSSEData writes one SSE "data" event with the given payload.
func sendSSEData(w http.ResponseWriter, flusher http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}

// sendSSEEvent writes a named SSE event (e.g. "error") with payload.
func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, name, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
	flusher.Flush()
}
