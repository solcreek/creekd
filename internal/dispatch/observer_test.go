package dispatch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestResponseObserverFires confirms the observer hook is invoked
// once per dispatched request with the bytes sent + final status.
// The wrap must preserve all stdlib http.ResponseWriter behaviour;
// upstream writes flow through unchanged.
func TestResponseObserverFires(t *testing.T) {
	// Upstream replies 201 + 4-byte body.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("body"))
	}))
	defer upstream.Close()

	port := portFromURL(t, upstream.URL)
	r := NewRouter()
	if err := r.SetAddr("hi", "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}

	var (
		gotApp    string
		gotBytes  int64
		gotStatus int
		calls     int32
	)
	r.SetObserver(func(appID string, bytesOut int64, statusCode int) {
		atomic.AddInt32(&calls, 1)
		gotApp = appID
		gotBytes = bytesOut
		gotStatus = statusCode
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderAppID, "hi")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if c := atomic.LoadInt32(&calls); c != 1 {
		t.Fatalf("observer called %d times, want 1", c)
	}
	if gotApp != "hi" {
		t.Errorf("appID = %q, want hi", gotApp)
	}
	if gotBytes != 4 {
		t.Errorf("bytes = %d, want 4", gotBytes)
	}
	if gotStatus != http.StatusCreated {
		t.Errorf("status = %d, want %d", gotStatus, http.StatusCreated)
	}
	if rr.Code != http.StatusCreated || rr.Body.String() != "body" {
		t.Errorf("response broken by wrap: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// TestResponseObserverNilHook: with no observer set, the wrap path
// is skipped entirely. Asserted via the absence of any work being
// done (mostly a regression guard — earlier drafts always wrapped,
// which added a malloc per request even when nobody cared).
func TestResponseObserverNilHook(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	port := portFromURL(t, upstream.URL)
	r := NewRouter()
	if err := r.SetAddr("hi", "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}
	// observer intentionally not set.

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderAppID, "hi")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK || rr.Body.String() != "ok" {
		t.Errorf("response broken: code=%d body=%q", rr.Code, rr.Body.String())
	}
}

// TestResponseObserverConcurrent: many goroutines dispatching through
// the same router with an observer set should record the right total
// bytes. Guards against torn counter reads / lost updates inside the
// wrap layer.
func TestResponseObserverConcurrent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("xx"))
	}))
	defer upstream.Close()

	port := portFromURL(t, upstream.URL)
	r := NewRouter()
	if err := r.SetAddr("c", "127.0.0.1", port); err != nil {
		t.Fatal(err)
	}

	var totalBytes int64
	r.SetObserver(func(_ string, bytesOut int64, _ int) {
		atomic.AddInt64(&totalBytes, bytesOut)
	})

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set(HeaderAppID, "c")
			rr := httptest.NewRecorder()
			r.ServeHTTP(rr, req)
		}()
	}
	wg.Wait()

	if want := int64(N * 2); totalBytes != want {
		t.Errorf("totalBytes = %d, want %d", totalBytes, want)
	}
}

// portFromURL extracts the port from a "http://127.0.0.1:PORT" URL.
// Helper for httptest-based wiring.
func portFromURL(t *testing.T, u string) int {
	t.Helper()
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		t.Fatalf("unexpected upstream url %q", u)
	}
	tail := strings.TrimPrefix(u, prefix)
	var n int
	for _, ch := range tail {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	if n == 0 {
		t.Fatalf("could not parse port from %q", u)
	}
	return n
}
