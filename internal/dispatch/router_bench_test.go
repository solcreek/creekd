package dispatch

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// BenchmarkRouterServeHTTP measures the per-request overhead of the
// router's lookup + httputil.ReverseProxy round-trip against a
// no-op backend. Useful as a baseline for "how cheap is dispatch?"
// — anything we add to the hot path (logging, metrics, policy)
// should be visible here.
//
// Run: go test -bench=BenchmarkRouterServeHTTP -benchmem ./internal/dispatch/
func BenchmarkRouterServeHTTP(b *testing.B) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	r := NewRouter()
	port, err := portFromHTTPTestURL(backend.URL)
	if err != nil {
		b.Fatalf("port: %v", err)
	}
	if err := r.SetAddr("app", "127.0.0.1", port); err != nil {
		b.Fatalf("SetAddr: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderAppID, "app")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 200 {
			b.Fatalf("status = %d", w.Code)
		}
	}
}

// BenchmarkRouterSet exercises the registration hot path: every
// admin Spawn lands here. Concurrent contention is rare in practice
// (Spawn is human-paced) but the bench guards against regression on
// the RWMutex path.
func BenchmarkRouterSet(b *testing.B) {
	r := NewRouter()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.Set(fmt.Sprintf("a%d", i), 9000+(i%50000)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRouterGet measures the lookup-only path — every incoming
// data-plane request hits this. A pre-populated router with N routes
// covers the common case (RWMutex with no writers in flight).
func BenchmarkRouterGet(b *testing.B) {
	r := NewRouter()
	for i := 0; i < 100; i++ {
		_ = r.Set(fmt.Sprintf("a%d", i), 9000+i)
	}
	keys := make([]string, 100)
	for i := range keys {
		keys[i] = fmt.Sprintf("a%d", i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.Get(keys[i%len(keys)])
	}
}

// BenchmarkRouterParallelGet measures Get under read contention —
// the realistic data-plane shape where many goroutines proxy
// concurrently.
func BenchmarkRouterParallelGet(b *testing.B) {
	r := NewRouter()
	for i := 0; i < 100; i++ {
		_ = r.Set(fmt.Sprintf("a%d", i), 9000+i)
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			_ = r.Get(fmt.Sprintf("a%d", i%100))
			i++
		}
	})
}

// BenchmarkProxyThroughputBytes measures full-request bytes/sec by
// proxying a 4 KiB body. b.SetBytes lets `go test -bench` report
// MB/s alongside ns/op.
func BenchmarkProxyThroughputBytes(b *testing.B) {
	body := strings.Repeat("x", 4096)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
	}))
	defer backend.Close()

	r := NewRouter()
	port, _ := portFromHTTPTestURL(backend.URL)
	_ = r.SetAddr("app", "127.0.0.1", port)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(HeaderAppID, "app")

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}
}

// portFromHTTPTestURL extracts the integer port from a
// httptest.Server URL like "http://127.0.0.1:54321".
func portFromHTTPTestURL(rawURL string) (int, error) {
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(rawURL, prefix) {
		return 0, fmt.Errorf("unexpected URL %q", rawURL)
	}
	var p int
	if _, err := fmt.Sscanf(rawURL[len(prefix):], "%d", &p); err != nil {
		return 0, err
	}
	return p, nil
}
