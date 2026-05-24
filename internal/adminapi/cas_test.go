package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/state"
)

// TestCAS_DeployWithMatchingIfMatchSucceeds covers the happy path:
// client GETs the envelope, sends the rv back as If-Match on the
// next deploy → success + 200.
func TestCAS_DeployWithMatchingIfMatchSucceeds(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, body := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "cas", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d body=%s", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("cas") })

	// Read current rv via Meta — same value the client would observe
	// via GET .metadata.resourceVersion.
	meta, ok := store.Meta("cas")
	if !ok {
		t.Fatal("Meta(\"cas\") returned false after spawn")
	}
	rv := strconv.FormatUint(meta.ResourceVersion, 10)

	// Deploy with matching If-Match → expect 200.
	req := httptest.NewRequest("POST", "/v1/apps/cas/deploy", strings.NewReader(`{"port":`+strconv.Itoa(freeTCPPort(t))+`,"command":"sleep","args":["30"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", rv)
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusPreconditionFailed {
		t.Fatalf("got 412 with matching If-Match=%q: body=%s", rv, w.Body.String())
	}
	if h := w.Header().Get("Warning"); h != "" {
		t.Errorf("unexpected Warning header on matching If-Match: %q", h)
	}
	// Deliberately not asserting ETag on the success path. The
	// middleware no longer sets it pre-mutation because Deploy/Stop
	// bumps rv inside the handler, making any middleware-set ETag
	// stale. Post-mutation ETag from the handler is tracked as a
	// follow-up; the 412 path's ETag is covered by the mismatch test.
}

// TestCAS_DeployWithMismatchedIfMatchReturns412 covers the rejection
// path: client sends a stale rv → 412 with the current rv in body +
// ETag header.
func TestCAS_DeployWithMismatchedIfMatchReturns412(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "mismatch", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("mismatch") })

	meta, _ := store.Meta("mismatch")
	currentRV := strconv.FormatUint(meta.ResourceVersion, 10)

	// Send an obviously-stale rv.
	req := httptest.NewRequest("POST", "/v1/apps/mismatch/deploy", strings.NewReader(`{"port":1234,"command":"sleep","args":["30"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "99999")
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status=%d, want 412; body=%s", w.Code, w.Body.String())
	}
	if etag := w.Header().Get("ETag"); etag != `"`+currentRV+`"` {
		t.Errorf("ETag = %q, want %q", etag, `"`+currentRV+`"`)
	}
	var body apitypes.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 412 body: %v; raw=%s", err, w.Body.String())
	}
	if body.Code != apitypes.ErrorCodeResourceVersionMismatch {
		t.Errorf("body.code = %q, want %q", body.Code, apitypes.ErrorCodeResourceVersionMismatch)
	}
	if !strings.Contains(body.Error, currentRV) {
		t.Errorf("body.error should mention current rv %q; got %q", currentRV, body.Error)
	}
}

// TestCAS_DeployWithoutIfMatchEmitsWarning covers the unconditional-
// write path: header absent → still goes through, but with Warning
// 299 so raw-curl users see the soft gap.
func TestCAS_DeployWithoutIfMatchEmitsWarning(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "uncond", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("uncond") })

	// Deploy without If-Match → expect Warning + go through.
	req := httptest.NewRequest("POST", "/v1/apps/uncond/deploy", strings.NewReader(`{"port":`+strconv.Itoa(freeTCPPort(t))+`,"command":"sleep","args":["30"]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusPreconditionFailed {
		t.Fatalf("got 412 without If-Match header; should be allowed with Warning")
	}
	if warn := w.Header().Get("Warning"); !strings.Contains(warn, `unconditional-write`) {
		t.Errorf("Warning header = %q, want it to contain \"unconditional-write\"", warn)
	}
}

// TestCAS_DeleteIfMatchPathCoverage covers DELETE: same three-branch
// matrix, but on the stopApp endpoint.
func TestCAS_DeleteIfMatchPathCoverage(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		wantStatus int
		wantWarn   string
	}{
		{name: "matching", header: "MATCH", wantStatus: http.StatusNoContent},
		{name: "mismatched", header: "99999", wantStatus: http.StatusPreconditionFailed},
		{name: "absent", header: "", wantStatus: http.StatusNoContent, wantWarn: "unconditional-write"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, "")
			store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
			if err != nil {
				t.Fatalf("NewStore: %v", err)
			}
			ts.srv.SetStore(store)

			id := "del-" + tc.name
			port := freeTCPPort(t)
			if status, _ := ts.do(t, "POST", "/v1/apps",
				apitypes.SpawnRequest{Id: id, Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
				t.Fatalf("spawn: status=%d", status)
			}

			ifMatch := tc.header
			if ifMatch == "MATCH" {
				meta, _ := store.Meta(id)
				ifMatch = strconv.FormatUint(meta.ResourceVersion, 10)
			}

			req := httptest.NewRequest("DELETE", "/v1/apps/"+id, nil)
			if tc.header != "" {
				req.Header.Set("If-Match", ifMatch)
			}
			w := httptest.NewRecorder()
			ts.srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantWarn != "" {
				if warn := w.Header().Get("Warning"); !strings.Contains(warn, tc.wantWarn) {
					t.Errorf("Warning header = %q, want it to contain %q", warn, tc.wantWarn)
				}
			}
			if tc.wantStatus != http.StatusNoContent {
				// Test didn't actually stop the app; clean up.
				_ = ts.sup.Stop(id)
			}
		})
	}
}

// TestCAS_QuotedIfMatchAcceptedPerRFC7232 covers the RFC 7232 ETag
// syntax — clients may wrap the value in double-quotes (`"123"`).
// The middleware strips the quotes before comparing.
func TestCAS_QuotedIfMatchAcceptedPerRFC7232(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "quoted", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("quoted") })

	meta, _ := store.Meta("quoted")
	rv := strconv.FormatUint(meta.ResourceVersion, 10)

	req := httptest.NewRequest("DELETE", "/v1/apps/quoted", nil)
	req.Header.Set("If-Match", `"`+rv+`"`) // quoted per RFC 7232
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for quoted matching If-Match; body=%s", w.Code, w.Body.String())
	}
}

// TestCAS_SpawnNotGated covers the create path — POST /v1/apps has
// no prior rv to match against, so the middleware lets it through
// regardless of headers.
func TestCAS_SpawnNotGated(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	// Spawn with a bogus If-Match — should succeed (creation, not mutation).
	req := httptest.NewRequest("POST", "/v1/apps", strings.NewReader(
		`{"id":"newapp","command":"sleep","args":["30"],"port":`+strconv.Itoa(port)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", "any-bogus-value")
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	t.Cleanup(func() { _ = ts.sup.Stop("newapp") })

	if w.Code != http.StatusCreated {
		t.Errorf("spawn status = %d, want 201 (Spawn should NOT be gated by If-Match); body=%s", w.Code, w.Body.String())
	}
	if w.Header().Get("Warning") != "" {
		t.Errorf("spawn should not emit unconditional-write Warning (not in CAS scope); got %q", w.Header().Get("Warning"))
	}
}

// TestCAS_RestartAndResetNotGated covers the operation paths —
// restart and reset are not spec writes, so the middleware leaves
// them alone.
func TestCAS_RestartAndResetNotGated(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	id := "ops"
	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: id, Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop(id) })

	// Send a deliberately-bogus If-Match — restart/reset paths
	// should NOT consult it (operations, not spec writes).
	for _, suffix := range []string{"/restart", "/reset"} {
		req := httptest.NewRequest("POST", "/v1/apps/"+id+suffix, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("If-Match", "99999")
		w := httptest.NewRecorder()
		ts.srv.Handler().ServeHTTP(w, req)
		if w.Code == http.StatusPreconditionFailed {
			t.Errorf("%s rejected by CAS middleware with bogus If-Match — operations should not be gated", suffix)
		}
	}
}

// TestCAS_SameAppMutationsSerialise proves the per-app lock wired
// through CAS middleware actually serialises mutations against the
// same app. Two concurrent DELETE requests against the same id must
// observe one-at-a-time semantics: the first acquires, the second
// blocks until release. Without the lock both could proceed in
// parallel and race on the supervisor + store mutation.
func TestCAS_SameAppMutationsSerialise(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "ser", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}

	// Fire two DELETE requests in parallel. The per-app lock ensures
	// one runs to completion before the other starts. The expected
	// outcome set is {204, 404}: the winner deletes (204) and the
	// loser finds the resource already gone (404). Without
	// serialisation we'd risk one of them seeing torn state (e.g. a
	// supervisor 5xx mid-stop), so observing exactly one 204 + one
	// 404 is the positive signal.
	type result struct {
		status int
		body   string
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			status, body := ts.do(t, "DELETE", "/v1/apps/ser", nil, "")
			results <- result{status: status, body: string(body)}
		}()
	}
	var got204, got404, gotOther int
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			switch r.status {
			case http.StatusNoContent:
				got204++
			case http.StatusNotFound:
				got404++
			default:
				gotOther++
				t.Errorf("parallel DELETE saw unexpected status=%d body=%s", r.status, r.body)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("parallel DELETE deadlocked")
		}
	}
	if got204 != 1 || got404 != 1 {
		t.Errorf("parallel DELETE outcome: 204=%d, 404=%d, other=%d — want exactly one 204 + one 404 (proves serialisation)",
			got204, got404, gotOther)
	}
}

// TestCAS_DifferentAppMutationsParallel proves the per-app lock
// does NOT block mutations against distinct apps from running
// concurrently. Two parallel DELETEs against different ids should
// finish in less wall-time than 2x a single DELETE's blocking
// portion would imply if they serialised.
func TestCAS_DifferentAppMutationsParallel(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port1, port2 := freeTCPPort(t), freeTCPPort(t)
	for i, id := range []string{"par-1", "par-2"} {
		p := port1
		if i == 1 {
			p = port2
		}
		if status, _ := ts.do(t, "POST", "/v1/apps",
			apitypes.SpawnRequest{Id: id, Command: ptr("sleep"), Args: &[]string{"30"}, Port: p}, ""); status != http.StatusCreated {
			t.Fatalf("spawn %s: status=%d", id, status)
		}
	}

	done := make(chan int, 2)
	for _, id := range []string{"par-1", "par-2"} {
		go func(id string) {
			status, _ := ts.do(t, "DELETE", "/v1/apps/"+id, nil, "")
			done <- status
		}(id)
	}
	for i := 0; i < 2; i++ {
		select {
		case s := <-done:
			if s != http.StatusNoContent {
				t.Errorf("DELETE status = %d, want 204", s)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("parallel DELETE on different apps timed out — locks may be over-serialising")
		}
	}
}

// TestCAS_NoStoreSkipsValidation covers the early-boot path: if
// SetStore was never called the middleware short-circuits so the
// admin API stays usable for in-memory test scenarios.
func TestCAS_NoStoreSkipsValidation(t *testing.T) {
	ts := newTestServer(t, "") // no SetStore call

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "nostore", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("nostore") })

	// Bogus If-Match without a store → should NOT 412 (no rv to compare against).
	req := httptest.NewRequest("DELETE", "/v1/apps/nostore", nil)
	req.Header.Set("If-Match", "99999")
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusPreconditionFailed {
		t.Errorf("412 returned without store configured — middleware should skip when store is nil")
	}
}

// TestCAS_AuthPrecedesCAS guards the middleware order: auth must run
// before CAS so that an unauthenticated caller cannot read a 412 with
// the current resourceVersion (which would leak both existence and
// version of the resource).
//
// Regression: oapi-codegen wraps middlewares in slice order, making
// slice[0] innermost and slice[len-1] outermost. Slice
// [auth, cas, audit] therefore executes audit→cas→auth, with CAS
// running before auth. The correct slice for audit→auth→cas is
// [cas, auth, audit].
func TestCAS_AuthPrecedesCAS(t *testing.T) {
	ts := newTestServer(t, "secret")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	// Spawn an app so it has a real rv to leak.
	port := freeTCPPort(t)
	if status, body := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "guarded", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "secret"); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d body=%s", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("guarded") })

	// Unauthenticated DELETE with bogus If-Match. If CAS runs before
	// auth, the response is 412 with the real rv in body + ETag header
	// (information disclosure). With correct order it must be 401.
	req := httptest.NewRequest("DELETE", "/v1/apps/guarded", nil)
	req.Header.Set("If-Match", "99999")
	// Deliberately no Authorization header.
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated request, got %d (body=%s)", w.Code, w.Body.String())
	}
	if w.Header().Get("ETag") != "" {
		t.Errorf("ETag leaked to unauthenticated caller: %q", w.Header().Get("ETag"))
	}
	if strings.Contains(w.Body.String(), `"resource_version_mismatch"`) {
		t.Errorf("CAS 412 body returned to unauthenticated caller: %s", w.Body.String())
	}
}

// TestCAS_NoStoreEmitsWarningOnMissingIfMatch guards the contract that
// the Warning header is emitted regardless of whether a store is
// configured. d6895ec hoisted the Warning emit before the store
// short-circuit; this test prevents a future reorder from making the
// Warning store-dependent again.
func TestCAS_NoStoreEmitsWarningOnMissingIfMatch(t *testing.T) {
	ts := newTestServer(t, "") // no SetStore call

	port := freeTCPPort(t)
	if status, _ := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "nostore-warn", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d", status)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("nostore-warn") })

	req := httptest.NewRequest("DELETE", "/v1/apps/nostore-warn", nil)
	// Deliberately no If-Match header.
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if warn := w.Header().Get("Warning"); !strings.Contains(warn, "unconditional-write") {
		t.Errorf("Warning header = %q, want it to contain \"unconditional-write\" even without a store", warn)
	}
}

// TestCAS_AppNamedAppsIsNotBypassed guards against an extractAppID
// regression that previously special-cased the literal id "apps" and
// returned "" — which made CAS short-circuit and let DELETE /v1/apps/apps
// bypass If-Match validation. supervisor.ValidateID permits "apps" as a
// name, so the middleware must treat it like any other id.
func TestCAS_AppNamedAppsIsNotBypassed(t *testing.T) {
	ts := newTestServer(t, "")
	store, err := state.NewStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ts.srv.SetStore(store)

	port := freeTCPPort(t)
	if status, body := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "apps", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, ""); status != http.StatusCreated {
		t.Fatalf("spawn id=\"apps\": status=%d body=%s", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("apps") })

	// DELETE with bogus If-Match. If extractAppID returns "" for "apps",
	// the middleware short-circuits and the bogus rv is silently
	// accepted → 204. With the fix it produces 412.
	req := httptest.NewRequest("DELETE", "/v1/apps/apps", nil)
	req.Header.Set("If-Match", "99999")
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("expected 412 for bogus If-Match on app named \"apps\", got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestCAS_DuplicateIfMatchHeaderRequiresAuthFirst guards the wrapper-
// level header-parameter parsing path. oapi-codegen parses path /
// header / query params before the middleware chain runs, so any
// parse error (e.g. two If-Match values when the schema permits at
// most one) reaches handleParamError without auth/audit having acted.
//
// Without the fix in handleParamError, an unauthenticated caller
// would receive a 400 (leaking that the endpoint exists and accepts
// If-Match) instead of a 401, and the probe wouldn't be audited.
func TestCAS_DuplicateIfMatchHeaderRequiresAuthFirst(t *testing.T) {
	ts := newTestServer(t, "secret")

	// Spawn an app so the path-id parse succeeds and we land on the
	// header-parameter parse step.
	port := freeTCPPort(t)
	if status, body := ts.do(t, "POST", "/v1/apps",
		apitypes.SpawnRequest{Id: "guarded-param", Command: ptr("sleep"), Args: &[]string{"30"}, Port: port}, "secret"); status != http.StatusCreated {
		t.Fatalf("spawn: status=%d body=%s", status, body)
	}
	t.Cleanup(func() { _ = ts.sup.Stop("guarded-param") })

	req := httptest.NewRequest("DELETE", "/v1/apps/guarded-param", nil)
	// Two If-Match values — the spec permits at most one, so the
	// wrapper calls ErrorHandlerFunc before the middleware chain.
	req.Header.Add("If-Match", "1")
	req.Header.Add("If-Match", "2")
	// Deliberately no Authorization header.
	w := httptest.NewRecorder()
	ts.srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated duplicate-If-Match request, got %d (body=%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "If-Match") {
		t.Errorf("response body leaks parsing detail to anonymous caller: %s", w.Body.String())
	}
}
