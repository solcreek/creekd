package dispatch

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// startBackend launches an httptest.Server whose /health response and
// every other path return the given signature. Caller must Close it.
func startBackend(t *testing.T, signature string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend-Signature", signature)
		w.WriteHeader(200)
		_, _ = w.Write([]byte(signature + ":" + r.URL.Path))
	}))
	return srv
}

// portOf returns the TCP port a httptest.Server is bound to.
func portOf(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	_, p, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split %s: %v", u.Host, err)
	}
	var n int
	if _, err := readInt(p, &n); err != nil {
		t.Fatalf("port parse %q: %v", p, err)
	}
	return n
}

func readInt(s string, dst *int) (int, error) {
	var n int
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, &numErr{s: s}
		}
		n = n*10 + int(s[i]-'0')
	}
	*dst = n
	return n, nil
}

type numErr struct{ s string }

func (e *numErr) Error() string { return "bad number: " + e.s }

// proxyGet performs a request against the router with the given app
// header and returns (status, body).
func proxyGet(t *testing.T, r *Router, appID, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", "http://creek.local"+path, nil)
	if appID != "" {
		req.Header.Set(HeaderAppID, appID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	res := w.Result()
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(body)
}

func TestRouterSetAndGet(t *testing.T) {
	r := NewRouter()
	if err := r.Set("a", 9000); err != nil {
		t.Fatalf("Set: %v", err)
	}
	b := r.Get("a")
	if b == nil {
		t.Fatal("Get returned nil")
	}
	if b.Port != 9000 {
		t.Errorf("Port = %d, want 9000", b.Port)
	}
	if b.AppID != "a" {
		t.Errorf("AppID = %q, want a", b.AppID)
	}
}

func TestRouterSetAddrUsesHost(t *testing.T) {
	r := NewRouter()
	if err := r.SetAddr("a", "10.42.0.5", 8080); err != nil {
		t.Fatalf("SetAddr: %v", err)
	}
	b := r.Get("a")
	if b == nil {
		t.Fatal("Get returned nil")
	}
	if b.Host != "10.42.0.5" {
		t.Errorf("Host = %q, want 10.42.0.5", b.Host)
	}
	if b.URL.Host != "10.42.0.5:8080" {
		t.Errorf("URL.Host = %q, want 10.42.0.5:8080", b.URL.Host)
	}
}

func TestRouterSetDefaultsToLoopback(t *testing.T) {
	r := NewRouter()
	_ = r.Set("a", 9000)
	b := r.Get("a")
	if b.Host != DefaultHost {
		t.Errorf("Host = %q, want %q", b.Host, DefaultHost)
	}
}

func TestRouterSetReplaces(t *testing.T) {
	r := NewRouter()
	_ = r.Set("a", 9000)
	_ = r.Set("a", 9001)
	if b := r.Get("a"); b == nil || b.Port != 9001 {
		t.Errorf("after replace: %+v", b)
	}
}

func TestRouterRemove(t *testing.T) {
	r := NewRouter()
	_ = r.Set("a", 9000)
	if !r.Remove("a") {
		t.Error("Remove returned false for existing route")
	}
	if r.Get("a") != nil {
		t.Error("Get returned non-nil after Remove")
	}
	if r.Remove("never-existed") {
		t.Error("Remove returned true for unknown route")
	}
}

func TestRouterSetValidatesArgs(t *testing.T) {
	r := NewRouter()
	if err := r.Set("", 9000); err == nil {
		t.Error("expected error for empty appID")
	}
	if err := r.Set("a", 0); err == nil {
		t.Error("expected error for port 0")
	}
	if err := r.Set("a", 100000); err == nil {
		t.Error("expected error for port out of range")
	}
}

func TestRouterSnapshotAndIDs(t *testing.T) {
	r := NewRouter()
	_ = r.Set("b", 9001)
	_ = r.Set("a", 9000)
	_ = r.Set("c", 9002)

	snap := r.Snapshot()
	want := map[string]int{"a": 9000, "b": 9001, "c": 9002}
	if !reflect.DeepEqual(snap, want) {
		t.Errorf("Snapshot = %v, want %v", snap, want)
	}
	if ids := r.IDs(); !reflect.DeepEqual(ids, []string{"a", "b", "c"}) {
		t.Errorf("IDs = %v, want sorted", ids)
	}
	// Mutating the snapshot doesn't affect router state.
	delete(snap, "a")
	if r.Get("a") == nil {
		t.Error("router state mutated via snapshot")
	}
}

func TestAppIDFromRequest(t *testing.T) {
	// Header wins.
	req := httptest.NewRequest("GET", "/?app=fromquery", nil)
	req.Header.Set(HeaderAppID, "fromheader")
	if got := AppIDFromRequest(req); got != "fromheader" {
		t.Errorf("got %q, want fromheader", got)
	}

	// Query fallback.
	req = httptest.NewRequest("GET", "/?app=fromquery", nil)
	if got := AppIDFromRequest(req); got != "fromquery" {
		t.Errorf("got %q, want fromquery", got)
	}

	// Neither.
	req = httptest.NewRequest("GET", "/", nil)
	if got := AppIDFromRequest(req); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestServeHTTPMissingAppIDReturns400(t *testing.T) {
	r := NewRouter()
	status, _ := proxyGet(t, r, "", "/anything")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestServeHTTPUnknownAppReturns503(t *testing.T) {
	r := NewRouter()
	status, _ := proxyGet(t, r, "missing-app", "/anything")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
}

func TestServeHTTPProxiesToBackend(t *testing.T) {
	srv := startBackend(t, "alpha")
	t.Cleanup(srv.Close)

	r := NewRouter()
	if err := r.Set("alpha", portOf(t, srv)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	status, body := proxyGet(t, r, "alpha", "/hello")
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "alpha:/hello") {
		t.Errorf("body = %q, want substring %q", body, "alpha:/hello")
	}
}

func TestServeHTTPRoutesByQueryFallback(t *testing.T) {
	srv := startBackend(t, "beta")
	t.Cleanup(srv.Close)

	r := NewRouter()
	_ = r.Set("beta", portOf(t, srv))

	req := httptest.NewRequest("GET", "http://creek.local/path?app=beta", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	res := w.Result()
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

// TestSetIsAtomicFlip simulates a blue-green flip while a stream of
// concurrent requests is in flight. After Set returns, no request
// should land on the old backend; before Set, none should land on the
// new one. (This is the "monotonic flip" guarantee the supervisor
// relies on for M5.7.)
func TestSetIsAtomicFlip(t *testing.T) {
	v1 := startBackend(t, "v1")
	t.Cleanup(v1.Close)
	v2 := startBackend(t, "v2")
	t.Cleanup(v2.Close)

	r := NewRouter()
	_ = r.Set("app", portOf(t, v1))

	// A goroutine hammers the router. We log which version each call sees.
	var stop atomic.Bool
	var seen sync.Map // step int → version string
	var step atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			n := step.Add(1)
			_, body := proxyGet(t, r, "app", "/")
			version := "?"
			switch {
			case strings.HasPrefix(body, "v1"):
				version = "v1"
			case strings.HasPrefix(body, "v2"):
				version = "v2"
			}
			seen.Store(n, version)
		}
	}()

	// Let some traffic flow against v1.
	time.Sleep(50 * time.Millisecond)

	// Flip and record the step when Set returned.
	flipStep := step.Load() + 1
	if err := r.Set("app", portOf(t, v2)); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// More traffic against v2.
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	<-done

	// Validate: every step ≥ flipStep sees v2 (no time-travel back to v1)
	// and every step strictly before flipStep sees v1.
	seen.Range(func(k, v any) bool {
		n := k.(int64)
		version := v.(string)
		if n < flipStep && version != "v1" {
			t.Errorf("step %d before flip saw %s, want v1", n, version)
		}
		if n > flipStep && version != "v2" {
			t.Errorf("step %d after flip saw %s, want v2", n, version)
		}
		return true
	})
}

// TestBackendDownReturns502: if the registered port has no listener,
// the proxy reports 502 via the per-Backend ErrorHandler, not the
// default empty body.
func TestBackendDownReturns502(t *testing.T) {
	r := NewRouter()
	// Pick a port and immediately close to ensure no one is listening.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	_ = r.Set("dead", port)

	status, body := proxyGet(t, r, "dead", "/")
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
	if !strings.Contains(body, "dead") {
		t.Errorf("body should mention app id %q, got %q", "dead", body)
	}
}

// TestBackendDownConcurrentServeIsRaceFree hammers the same down
// backend from many goroutines. Pre-fix, ServeHTTP mutated the
// shared Backend.proxy.ErrorHandler on every request — race
// detector should flag concurrent writes to that field. After the
// fix, ErrorHandler is set once in newBackend and never touched
// again at serve time.
func TestBackendDownConcurrentServeIsRaceFree(t *testing.T) {
	r := NewRouter()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	if err := r.Set("dead", port); err != nil {
		t.Fatalf("Set: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, body := proxyGet(t, r, "dead", "/")
			if status != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", status)
			}
			if !strings.Contains(body, "dead") {
				t.Errorf("body should mention app id, got %q", body)
			}
		}()
	}
	wg.Wait()
}

func TestRouterConcurrentSetGetRace(t *testing.T) {
	r := NewRouter()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = r.Set("a", 9000+i)
		}(i)
		go func() {
			defer wg.Done()
			_ = r.Get("a")
		}()
	}
	wg.Wait()
	if r.Get("a") == nil {
		t.Error("expected route present after concurrent writes")
	}
}
