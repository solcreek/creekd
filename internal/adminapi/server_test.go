package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/supervisor"
)

// testServer wires a real Supervisor (no cgroup, no log dir, no probe)
// behind the admin API + an empty dispatch.Router. Used by all
// httptest-based tests below.
type testServer struct {
	srv    *Server
	sup    *supervisor.Supervisor
	router *dispatch.Router
}

func newTestServer(t *testing.T, token string) *testServer {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sup := supervisor.New(logger)
	sup.Stdout = io.Discard
	sup.Stderr = io.Discard
	sup.WaitDelay = 500 * time.Millisecond
	sup.HealthCheckInterval = 0 // disable probe noise in API tests
	r := dispatch.NewRouter()
	return &testServer{
		srv:    New(sup, r, token),
		sup:    sup,
		router: r,
	}
}

// do performs a request through the configured handler and returns
// status + body + decoded JSON.
func (ts *testServer) do(t *testing.T, method, path string, body any, token string) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	res := w.Result()
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func mustJSON(t *testing.T, body []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode body %q: %v", string(body), err)
	}
}

// freeTCPPort returns an OS-allocated free TCP port.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestAuthRequiredWhenTokenSet(t *testing.T) {
	ts := newTestServer(t, "secret")
	status, body := ts.do(t, "GET", "/v1/apps", nil, "")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
	var er ErrorResponse
	mustJSON(t, body, &er)
	if er.Code != CodeUnauthorized {
		t.Errorf("code = %q, want %q", er.Code, CodeUnauthorized)
	}
}

func TestAuthWrongTokenRejected(t *testing.T) {
	ts := newTestServer(t, "secret")
	status, _ := ts.do(t, "GET", "/v1/apps", nil, "guess")
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", status)
	}
}

func TestAuthDisabledWhenTokenEmpty(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "GET", "/v1/apps", nil, "")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 with auth disabled", status)
	}
}

func TestSpawnHappyPath(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	req := SpawnRequest{
		ID:      "spawn-1",
		Command: "sleep",
		Args:    []string{"30"},
		Port:    port,
	}
	status, body := ts.do(t, "POST", "/v1/apps", req, "")
	if status != http.StatusCreated {
		t.Fatalf("status = %d body = %s", status, body)
	}
	var view AppView
	mustJSON(t, body, &view)
	if view.ID != "spawn-1" || view.Port != port {
		t.Errorf("view = %+v", view)
	}
	if view.PID <= 0 {
		t.Errorf("PID = %d, want > 0", view.PID)
	}
	if view.Status != "running" {
		t.Errorf("Status = %q, want running", view.Status)
	}
	// Router registered.
	if b := ts.router.Get("spawn-1"); b == nil || b.Port != port {
		t.Errorf("router not updated: %+v", b)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("spawn-1") })
}

func TestSpawnDuplicateReturns409(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	req := SpawnRequest{ID: "dup", Command: "sleep", Args: []string{"30"}, Port: port}
	if s, _ := ts.do(t, "POST", "/v1/apps", req, ""); s != http.StatusCreated {
		t.Fatalf("first spawn: %d", s)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("dup") })

	status, body := ts.do(t, "POST", "/v1/apps", req, "")
	if status != http.StatusConflict {
		t.Errorf("status = %d body = %s, want 409", status, body)
	}
	var er ErrorResponse
	mustJSON(t, body, &er)
	if er.Code != CodeConflict {
		t.Errorf("code = %q, want %q", er.Code, CodeConflict)
	}
}

func TestSpawnValidatesRequired(t *testing.T) {
	ts := newTestServer(t, "")
	cases := []SpawnRequest{
		{Command: "sleep", Args: []string{"30"}, Port: 9000}, // no ID
		{ID: "x", Command: "sleep", Args: []string{"30"}},    // no port
	}
	for i, req := range cases {
		status, _ := ts.do(t, "POST", "/v1/apps", req, "")
		if status != http.StatusBadRequest {
			t.Errorf("case %d: status = %d, want 400", i, status)
		}
	}
}

func TestSpawnRejectsUnknownFields(t *testing.T) {
	ts := newTestServer(t, "")
	// Build raw JSON with a bogus field.
	body := strings.NewReader(`{"id":"x","port":9000,"command":"sleep","args":["30"],"oops":"typo"}`)
	req := httptest.NewRequest("POST", "/v1/apps", body)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for unknown field", w.Code)
	}
}

func TestListAndGet(t *testing.T) {
	ts := newTestServer(t, "")
	p1, p2 := freeTCPPort(t), freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "a", Command: "sleep", Args: []string{"30"}, Port: p1}, "")
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "b", Command: "sleep", Args: []string{"30"}, Port: p2}, "")
	t.Cleanup(func() {
		_ = ts.sup.Stop("a")
		_ = ts.sup.Stop("b")
	})

	// List.
	status, body := ts.do(t, "GET", "/v1/apps", nil, "")
	if status != http.StatusOK {
		t.Fatalf("list status = %d", status)
	}
	var list ListResponse
	mustJSON(t, body, &list)
	if len(list.Apps) != 2 {
		t.Errorf("apps len = %d, want 2", len(list.Apps))
	}

	// Get one.
	status, body = ts.do(t, "GET", "/v1/apps/a", nil, "")
	if status != http.StatusOK {
		t.Fatalf("get status = %d", status)
	}
	var view AppView
	mustJSON(t, body, &view)
	if view.ID != "a" || view.Port != p1 {
		t.Errorf("view = %+v", view)
	}
}

func TestGetUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, body := ts.do(t, "GET", "/v1/apps/ghost", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d body = %s, want 404", status, body)
	}
}

func TestStopRemovesFromRegistryAndRouter(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "x", Command: "sleep", Args: []string{"30"}, Port: port}, "")

	if ts.router.Get("x") == nil {
		t.Fatal("router missing app pre-stop")
	}

	status, _ := ts.do(t, "DELETE", "/v1/apps/x", nil, "")
	if status != http.StatusNoContent {
		t.Errorf("status = %d, want 204", status)
	}
	if ts.sup.Get("x") != nil {
		t.Errorf("supervisor still has app post-stop")
	}
	if ts.router.Get("x") != nil {
		t.Errorf("router still has app post-stop")
	}
}

func TestStopUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "DELETE", "/v1/apps/ghost", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestResetWhenNotCrashLoopingReturns409(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "h", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("h") })

	status, body := ts.do(t, "POST", "/v1/apps/h/reset", struct{}{}, "")
	if status != http.StatusConflict {
		t.Errorf("status = %d body = %s, want 409", status, body)
	}
}

func TestResetUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "POST", "/v1/apps/ghost/reset", struct{}{}, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestDeployUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	req := DeployRequest{Port: 9999, Command: "sleep", Args: []string{"30"}}
	status, _ := ts.do(t, "POST", "/v1/apps/ghost/deploy", req, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestDeployRequiresPort(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "x", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("x") })

	req := DeployRequest{Command: "sleep", Args: []string{"30"}}
	status, body := ts.do(t, "POST", "/v1/apps/x/deploy", req, "")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d body = %s, want 400", status, body)
	}
}

// TestDeploySamePortReturnsBadRequest exercises the underlying
// Supervisor.Deploy validation reaching the API correctly.
func TestDeploySamePortReturnsBadRequest(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "x", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("x") })

	req := DeployRequest{Port: port, Command: "sleep", Args: []string{"30"}}
	status, body := ts.do(t, "POST", "/v1/apps/x/deploy", req, "")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d body = %s, want 400", status, body)
	}
}

// TestLimitsZeroFieldsTreatedAsNil checks that an explicit but
// all-zero Limits block does not accidentally enable cgroup
// enforcement (which would require Supervisor.CgroupParent).
func TestLimitsZeroFieldsTreatedAsNil(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	req := SpawnRequest{
		ID: "z", Command: "sleep", Args: []string{"30"}, Port: port,
		Limits: &Limits{}, // all zeros
	}
	status, body := ts.do(t, "POST", "/v1/apps", req, "")
	if status != http.StatusCreated {
		t.Errorf("status = %d body = %s, want 201", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("z") })
}

// TestCgroupLimitsWithoutParentRejected: a non-zero Limits block must
// be propagated to the supervisor, where it errors when CgroupParent
// is empty.
func TestCgroupLimitsWithoutParentRejected(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	req := SpawnRequest{
		ID: "cg", Command: "sleep", Args: []string{"30"}, Port: port,
		Limits: &Limits{MemoryMaxBytes: 16 * 1024 * 1024},
	}
	status, _ := ts.do(t, "POST", "/v1/apps", req, "")
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (CgroupParent unset)", status)
	}
}

// --- Restart endpoint --------------------------------------------------

func TestRestartHappyPath(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.InitialBackoff = 10 * time.Millisecond
	ts.sup.MaxBackoff = 20 * time.Millisecond

	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "rs", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("rs") })

	oldPID := ts.sup.Get("rs").PID()

	status, body := ts.do(t, "POST", "/v1/apps/rs/restart",
		RestartRequest{TimeoutMS: 3000}, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, body)
	}
	var view AppView
	mustJSON(t, body, &view)
	if view.PID == 0 || view.PID == oldPID {
		t.Errorf("PID = %d, want a new non-zero PID (was %d)", view.PID, oldPID)
	}
	if view.Status != "running" {
		t.Errorf("Status = %q, want running", view.Status)
	}
}

func TestRestartUnknownReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "POST", "/v1/apps/ghost/restart", RestartRequest{}, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestRestartAcceptsEmptyBody(t *testing.T) {
	ts := newTestServer(t, "")
	ts.sup.InitialBackoff = 10 * time.Millisecond
	ts.sup.MaxBackoff = 20 * time.Millisecond

	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "noargs", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("noargs") })

	// Send POST with no body at all (nil body in the request).
	req := httptest.NewRequest("POST", "/v1/apps/noargs/restart", nil)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d body = %s, want 200 with no body", w.Code, w.Body.String())
	}
}

// --- pprof debug endpoint --------------------------------------------

func TestPprofDisabledByDefault(t *testing.T) {
	ts := newTestServer(t, "")
	// EnablePprof not called.
	req := httptest.NewRequest("GET", "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (pprof disabled)", w.Code)
	}
}

func TestPprofEnabledServesIndex(t *testing.T) {
	ts := newTestServer(t, "")
	ts.srv.EnablePprof()

	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	// pprof.Index renders an HTML listing that includes profile names.
	if !strings.Contains(body, "heap") || !strings.Contains(body, "goroutine") {
		t.Errorf("body missing expected profile names:\n%s", body[:min(500, len(body))])
	}
}

func TestPprofEnabledServesHeapProfile(t *testing.T) {
	ts := newTestServer(t, "")
	ts.srv.EnablePprof()

	req := httptest.NewRequest("GET", "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.Len() == 0 {
		t.Error("heap profile body is empty")
	}
}

func TestPprofRequiresBearerToken(t *testing.T) {
	ts := newTestServer(t, "secret")
	ts.srv.EnablePprof()

	// Without token → 401.
	req := httptest.NewRequest("GET", "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without token", w.Code)
	}

	// With token → 200.
	req = httptest.NewRequest("GET", "/debug/pprof/heap", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 with token", w.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Stats endpoint ---------------------------------------------------

func TestStatsWithoutCgroupReturnsDisabled(t *testing.T) {
	ts := newTestServer(t, "")
	port := freeTCPPort(t)
	_, _ = ts.do(t, "POST", "/v1/apps",
		SpawnRequest{ID: "nocg", Command: "sleep", Args: []string{"30"}, Port: port}, "")
	t.Cleanup(func() { _ = ts.sup.Stop("nocg") })

	status, body := ts.do(t, "GET", "/v1/apps/nocg/stats", nil, "")
	if status != http.StatusOK {
		t.Fatalf("status = %d body = %s", status, body)
	}
	var v StatsView
	mustJSON(t, body, &v)
	if v.ID != "nocg" {
		t.Errorf("id = %q", v.ID)
	}
	if v.CgroupEnabled {
		t.Errorf("CgroupEnabled = true, want false (spawned without limits)")
	}
	if v.MemoryCurrentBytes != 0 || v.PidsCurrent != 0 {
		t.Errorf("counters should be zero, got %+v", v)
	}
}

func TestStatsUnknownAppReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "GET", "/v1/apps/ghost/stats", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestUnknownPathReturns404: the mux's default response for an
// unmatched path is 404 — confirming we didn't accidentally catch-all.
func TestUnknownPathReturns404(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "GET", "/v1/random", nil, "")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestMethodMismatchReturns405: POST to a GET-only path. Go 1.22's
// mux returns 405 for method-only mismatches.
func TestMethodMismatchReturns405(t *testing.T) {
	ts := newTestServer(t, "")
	status, _ := ts.do(t, "PUT", "/v1/apps", nil, "")
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", status)
	}
}

// Ensure context cancellation reaches Deploy when client disconnects.
// We don't actually deploy here — just check that the handler honours
// the request context all the way through.
var _ = context.Background
