package adminapi

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
)

// writeLogFile writes a current.log file at <dir>/<appID>/ containing
// one record per line. Used by the unit tests below to drive readTail
// without needing a real supervised child.
func writeLogFile(t *testing.T, dir, appID string, lines []string) string {
	t.Helper()
	d := filepath.Join(dir, appID)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(d, "current.log")
	body := strings.Join(lines, "\n")
	if len(lines) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestReadTailReturnsRequestedSlice(t *testing.T) {
	dir := t.TempDir()
	all := []string{"a", "b", "c", "d", "e"}
	writeLogFile(t, dir, "app", all)

	got, err := readTail(filepath.Join(dir, "app", "current.log"), 3)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if want := []string{"c", "d", "e"}; !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadTailReturnsAllWhenNLargerThanFile(t *testing.T) {
	dir := t.TempDir()
	writeLogFile(t, dir, "app", []string{"a", "b"})

	got, err := readTail(filepath.Join(dir, "app", "current.log"), 10)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if want := []string{"a", "b"}; !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestReadTailEmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeLogFile(t, dir, "app", nil)

	got, err := readTail(filepath.Join(dir, "app", "current.log"), 10)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestReadTailMissingFileIsEmpty(t *testing.T) {
	got, err := readTail("/nonexistent/path", 10)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestReadTailZeroN(t *testing.T) {
	dir := t.TempDir()
	writeLogFile(t, dir, "app", []string{"a", "b", "c"})

	got, _ := readTail(filepath.Join(dir, "app", "current.log"), 0)
	if len(got) != 0 {
		t.Errorf("got %v, want empty for n=0", got)
	}
}

func TestLogsEndpointTailMode(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.LogDir = t.TempDir()

	// Spawn an app first so the path is recognised.
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "logged", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("logged") })

	// Pre-seed the log file (the sleep child won't emit anything itself).
	writeLogFile(t, ts.sup.LogDir, "logged",
		[]string{`{"ts":"t","app":"logged","stream":"stdout","msg":"one"}`,
			`{"ts":"t","app":"logged","stream":"stdout","msg":"two"}`,
			`{"ts":"t","app":"logged","stream":"stdout","msg":"three"}`})

	status, body := ts.do(t, "GET", "/v1/apps/logged/logs?tail=2", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, body)
	}
	bodyStr := string(body)
	if !strings.Contains(bodyStr, `"msg":"two"`) ||
		!strings.Contains(bodyStr, `"msg":"three"`) {
		t.Errorf("body missing tail content: %q", bodyStr)
	}
	if strings.Contains(bodyStr, `"msg":"one"`) {
		t.Errorf("body should not contain pre-tail line: %q", bodyStr)
	}
}

func TestLogsEndpointDefaultTail(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.LogDir = t.TempDir()

	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "dflt", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("dflt") })

	// 250 lines — more than defaultTail (100).
	lines := make([]string, 250)
	for i := range lines {
		lines[i] = fmt.Sprintf(`{"msg":"l%d"}`, i)
	}
	writeLogFile(t, ts.sup.LogDir, "dflt", lines)

	status, body := ts.do(t, "GET", "/v1/apps/dflt/logs", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	got := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(got) != defaultTail {
		t.Errorf("returned %d lines, want %d", len(got), defaultTail)
	}
	// First returned line must be l150 (250 - 100 = 150).
	if !strings.Contains(got[0], `"l150"`) {
		t.Errorf("first line = %q, want l150", got[0])
	}
	if !strings.Contains(got[len(got)-1], `"l249"`) {
		t.Errorf("last line = %q, want l249", got[len(got)-1])
	}
}

func TestLogsUnknownAppReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.LogDir = t.TempDir()
	status, _ := ts.do(t, "GET", "/v1/apps/ghost/logs", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestLogsCaptureDisabledReturns400(t *testing.T) {
	ts := newTestServer(t, "")
	// LogDir intentionally empty.
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "nolog", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("nolog") })

	status, body := ts.do(t, "GET", "/v1/apps/nolog/logs", nil, "")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d body = %s, want 400", status, body)
	}
}

func TestLogsRejectsBadTailParam(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.LogDir = t.TempDir()
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "x", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("x") })

	status, _ := ts.do(t, "GET", "/v1/apps/x/logs?tail=abc", nil, "")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// TestLogsFollowModeStreamsAppendedLines exercises the SSE follow
// path end-to-end via httptest.NewServer (httptest.Recorder cannot
// flush a streaming response cleanly).
func TestLogsFollowModeStreamsAppendedLines(t *testing.T) {
	// Speed up the poll interval for the test.
	old := followPollInterval
	followPollInterval = 50 * time.Millisecond
	defer func() { followPollInterval = old }()

	ts := newTestServer(t, "")
	ts.sup.LogDir = t.TempDir()
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "follow", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("follow") })

	// Pre-existing tail.
	logPath := writeLogFile(t, ts.sup.LogDir, "follow",
		[]string{`{"msg":"pre1"}`, `{"msg":"pre2"}`})

	httpSrv := ts.liveServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET",
		httpSrv.URL+"/v1/apps/follow/logs?tail=2&follow=true", nil)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream*", got)
	}

	// Read events in a goroutine; track received messages.
	type recv struct {
		mu   sync.Mutex
		seen []string
	}
	r := &recv{}
	var done int32
	go func() {
		defer atomic.StoreInt32(&done, 1)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				r.mu.Lock()
				r.seen = append(r.seen, line[len("data: "):])
				r.mu.Unlock()
			}
		}
	}()

	// Wait for the tail (pre1, pre2) to be received.
	if !waitFor(2*time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.seen) >= 2
	}) {
		r.mu.Lock()
		defer r.mu.Unlock()
		t.Fatalf("initial tail not received; got %v", r.seen)
	}

	// Append a new line to the file.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open append: %v", err)
	}
	if _, err := io.WriteString(f, `{"msg":"new"}`+"\n"); err != nil {
		t.Fatalf("write append: %v", err)
	}
	f.Close()

	if !waitFor(3*time.Second, func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, s := range r.seen {
			if strings.Contains(s, `"new"`) {
				return true
			}
		}
		return false
	}) {
		r.mu.Lock()
		defer r.mu.Unlock()
		t.Fatalf("appended line not received; got %v", r.seen)
	}

	cancel() // signal client disconnect
	// Give the streaming goroutine a moment to exit so we don't race
	// the t.Cleanup → server shutdown ordering.
	waitFor(500*time.Millisecond, func() bool { return atomic.LoadInt32(&done) == 1 })
}

func waitFor(timeout time.Duration, f func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return f()
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
