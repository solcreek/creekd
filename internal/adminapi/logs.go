package adminapi

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
)

const defaultTail = 100
const maxTail = 10_000

var followPollInterval = 500 * time.Millisecond

func tailLogs(w http.ResponseWriter, path string, n int) {
	lines, err := readTail(path, n)
	if err != nil {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	for _, l := range lines {
		_, _ = w.Write([]byte(l))
		_, _ = w.Write([]byte{'\n'})
	}
}

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

func streamLogs(w http.ResponseWriter, r *http.Request, path string, initialTail int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
			"streaming not supported by underlying writer")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	initial, err := readTail(path, initialTail)
	if err != nil {
		sendSSEEvent(w, flusher, "error", err.Error())
		return
	}
	for _, line := range initial {
		sendSSEData(w, flusher, line)
	}

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
			continue
		}
		size := info.Size()
		if size < pos {
			pos = 0
		}
		if size == pos {
			continue
		}

		appended, newPos, err := readFromOffset(path, pos)
		if err != nil {
			continue
		}
		for _, line := range appended {
			sendSSEData(w, flusher, line)
		}
		pos = newPos
	}
}

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

func sendSSEData(w http.ResponseWriter, flusher http.Flusher, payload string) {
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	flusher.Flush()
}

func sendSSEEvent(w http.ResponseWriter, flusher http.Flusher, name, payload string) {
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
	flusher.Flush()
}
