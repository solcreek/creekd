package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/solcreek/creekd/internal/apitypes"
)

// casMiddleware validates the `If-Match` header against the
// resource's current `resourceVersion` for spec-mutating endpoints.
//
// Scope:
//   - DELETE /v1/apps/{id}           — applies
//   - POST   /v1/apps/{id}/deploy    — applies
//
// Not in scope:
//   - POST /v1/apps                  — Spawn / create has no prior rv to match.
//   - POST /v1/apps/{id}/restart     — operation, not a spec mutation.
//   - POST /v1/apps/{id}/reset       — operation, not a spec mutation.
//   - Status reads, list, logs       — non-mutating.
//
// Behaviour per DESIGN-self-host-state.md §"First-party CLI MUST
// send If-Match":
//   - Header present + matches current rv → continue.
//   - Header present + mismatch → 412 Precondition Failed; response
//     body carries the current rv so the client can retry. The
//     `ETag` response header is also set for HTTP-native clients.
//   - Header absent → continue, but emit `Warning: 299 -
//     "unconditional-write"` so raw-curl users see the gap. v6's
//     long-term position is to mark PUT verbs `REQUIRED` once a
//     wire-level PUT endpoint lands; creekd's current POST verbs
//     are treated as PATCH-equivalent (optional).
//   - Store not configured (early-boot or test paths) → skip the
//     check entirely. Same for resources the store has no metadata
//     for (in-memory-only supervisor state).
func (s *Server) casMiddleware() apitypes.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !casApplies(r) {
				next.ServeHTTP(w, r)
				return
			}
			id := extractAppID(r.URL.Path)
			if id == "" || s.store == nil {
				next.ServeHTTP(w, r)
				return
			}
			meta, ok := s.store.Meta(id)
			if !ok {
				// Resource doesn't have persisted metadata. Downstream
				// handler will surface 404 — no rv to compare against
				// here, so let it through.
				next.ServeHTTP(w, r)
				return
			}
			currentRV := strconv.FormatUint(meta.ResourceVersion, 10)
			ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
			if ifMatch == "" {
				w.Header().Set("Warning", `299 - "unconditional-write"`)
				next.ServeHTTP(w, r)
				return
			}
			// Strip optional double-quotes per RFC 7232 ETag syntax
			// (`"123"`); creekd's rv is an opaque string but tolerate
			// both quoted + unquoted client forms.
			ifMatch = strings.Trim(ifMatch, `"`)
			if ifMatch != currentRV {
				writePreconditionFailed(w, currentRV, ifMatch)
				return
			}
			// Match — set ETag header for HTTP-native clients that
			// want to see the canonical rv on the response too.
			w.Header().Set("ETag", `"`+currentRV+`"`)
			next.ServeHTTP(w, r)
		})
	}
}

// casApplies returns true if the request is a spec-mutating call
// that should validate If-Match.
func casApplies(r *http.Request) bool {
	path := r.URL.Path
	switch r.Method {
	case http.MethodDelete:
		// DELETE /v1/apps/{id} — applies if id is present.
		return strings.HasPrefix(path, "/v1/apps/") && extractAppID(path) != ""
	case http.MethodPost:
		// POST /v1/apps/{id}/deploy — spec mutation. Restart and
		// reset are operations, not spec writes.
		return strings.HasSuffix(path, "/deploy") && extractAppID(path) != ""
	case http.MethodPut, http.MethodPatch:
		// Reserved for the future PUT/PATCH /v1/apps/{id} land.
		// When that endpoint exists it MUST require If-Match per
		// DESIGN; until then treat as applicable so the validator
		// is exercised on any caller already sending PUT.
		return strings.HasPrefix(path, "/v1/apps/") && extractAppID(path) != ""
	}
	return false
}

// writePreconditionFailed serialises the standard 412 response.
// Body is creekd's `{code, error}` shape (matches existing handler
// error responses, see writeError). ETag header carries the current
// rv so HTTP-native clients can read it without parsing the body.
func writePreconditionFailed(w http.ResponseWriter, currentRV, sent string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+currentRV+`"`)
	w.WriteHeader(http.StatusPreconditionFailed)
	_ = json.NewEncoder(w).Encode(apitypes.ErrorResponse{
		Code:  apitypes.ErrorCodeResourceVersionMismatch,
		Error: fmt.Sprintf("If-Match=%q does not match current resourceVersion=%q", sent, currentRV),
	})
}
