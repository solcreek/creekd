package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	if w.Header().Get("ETag") == "" {
		t.Error("ETag header missing on matching If-Match — clients expect it for HTTP-native rv tracking")
	}
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
