package supervisor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newHealthTestServer spins up an in-process HTTP server whose handler
// returns the configured status code for every route except "/specific"
// (which always returns 200) and "/strict-health" (also 200). Returns
// the App-shaped pieces the HealthChecker needs.
func newHealthTestServer(t *testing.T, defaultStatus int) (*App, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/specific", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/strict-health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(defaultStatus)
	})
	srv := httptest.NewServer(mux)

	u, _ := url.Parse(srv.URL)
	portStr := u.Port()
	port, _ := strconv.Atoi(portStr)

	app := &App{ID: "health-test", Port: port}
	return app, srv.Close
}

// TestHTTPHealthCheckerLenientAcceptsNon5xx: the lenient default
// must treat 200, 204, 301, 404, and other non-5xx as alive. This
// is the contract that makes the default safe for apps that don't
// expose /health — a fleet of apps with arbitrary URL maps should
// all pass the probe by default.
func TestHTTPHealthCheckerLenientAcceptsNon5xx(t *testing.T) {
	for _, status := range []int{200, 204, 301, 400, 404, 418} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			app, cleanup := newHealthTestServer(t, status)
			defer cleanup()

			h := &HTTPHealthChecker{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := h.Check(ctx, app); err != nil {
				t.Errorf("status %d: want nil, got %v", status, err)
			}
		})
	}
}

// TestHTTPHealthCheckerLenientFailsOn5xx: 5xx is the one and only
// HTTP status family that signals the server itself is broken
// (overload, internal error, bad gateway). Those should still
// count as failures so the supervisor restarts a genuinely sick
// process.
func TestHTTPHealthCheckerLenientFailsOn5xx(t *testing.T) {
	for _, status := range []int{500, 502, 503, 504} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			app, cleanup := newHealthTestServer(t, status)
			defer cleanup()

			h := &HTTPHealthChecker{}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := h.Check(ctx, app)
			if err == nil {
				t.Errorf("status %d: want error, got nil", status)
			} else if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("status %d: error %q should mention the status code", status, err)
			}
		})
	}
}

// TestHTTPHealthCheckerFailsOnConnectionRefused: connection refused
// is the canonical "process is dead" signal. The probe must fail
// here so the supervisor restarts.
func TestHTTPHealthCheckerFailsOnConnectionRefused(t *testing.T) {
	// Allocate then immediately release a port so it's almost
	// certainly free. Net/http listeners aren't required for this
	// case — we just need a port nothing is listening on.
	app := &App{ID: "x", Port: 1} // port 1 — always closed for non-root
	h := &HTTPHealthChecker{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := h.Check(ctx, app); err == nil {
		t.Error("want error on connection-refused, got nil")
	}
}

// TestHTTPHealthCheckerPerAppPathOverridesDefault: an app that wants
// strict readiness (only success on a specific endpoint) can set
// HealthCheckPath. The probe must hit that path, not the supervisor
// default. This is the path config flag's runtime semantics.
func TestHTTPHealthCheckerPerAppPathOverridesDefault(t *testing.T) {
	// Default route returns 500 (would fail under lenient mode).
	// /specific returns 200. The app's HealthCheckPath = "/specific"
	// must redirect the probe and make it pass.
	app, cleanup := newHealthTestServer(t, 500)
	defer cleanup()
	app.HealthCheckPath = "/specific"

	h := &HTTPHealthChecker{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Check(ctx, app); err != nil {
		t.Errorf("per-app path override: want nil, got %v", err)
	}
}

// TestHTTPHealthCheckerSupervisorDefaultPath: when the per-app field
// is empty, the supervisor-wide Path on the HealthChecker wins. This
// is the route a tenant-fleet operator uses to apply a uniform
// readiness contract (e.g. "every app in this fleet has /-/ready").
func TestHTTPHealthCheckerSupervisorDefaultPath(t *testing.T) {
	app, cleanup := newHealthTestServer(t, 500)
	defer cleanup()
	// app.HealthCheckPath intentionally empty.

	h := &HTTPHealthChecker{Path: "/strict-health"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := h.Check(ctx, app); err != nil {
		t.Errorf("supervisor default path: want nil, got %v", err)
	}
}
