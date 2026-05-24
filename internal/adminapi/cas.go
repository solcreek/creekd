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
// Behaviour per DESIGN-self-host-state.md §"First-party clients
// SHOULD send If-Match":
//   - Header present + matches current rv → continue.
//   - Header present + mismatch → 412 Precondition Failed; response
//     body carries the current rv so the client can retry. The
//     `ETag` response header is also set for HTTP-native clients.
//   - Header absent → continue, but emit `Warning: 299 -
//     "unconditional-write"` so raw-curl users see the gap. The
//     Warning is emitted regardless of whether the store is
//     configured or the resource has persisted metadata, because
//     it describes the *caller's* posture, not the server's. v6's
//     long-term position is to mark PUT verbs `REQUIRED` once a
//     wire-level PUT endpoint lands; creekd's current POST verbs
//     are treated as PATCH-equivalent (optional).
//   - Store not configured (early-boot or test paths) → skip rv
//     validation. Same for resources the store has no metadata
//     for (in-memory-only supervisor state).
//
// The first-party TS CLI (creek repo) is the reference client and
// sends If-Match automatically on Stop/Deploy. The Go creekctl /
// adminclient currently do not — tracked as a follow-up; until
// then, raw curl users and the Go CLI rely on the Warning header
// to know they're making an unconditional write.
//
// We deliberately do NOT set ETag on the success path here: by
// the time the handler completes (Deploy/Stop), the store will
// have bumped rv, making any pre-mutation ETag stale. The 412
// path's ETag is correct because no mutation happened. Handlers
// that want to expose the post-mutation rv should set ETag
// themselves after store.AddApp / store.RemoveApp.
func (s *Server) casMiddleware() apitypes.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !casApplies(r) {
				next.ServeHTTP(w, r)
				return
			}
			id := extractAppID(r.URL.Path)
			if id == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Per DESIGN §"Mutex granularity": acquire the per-app
			// write lock for the entire mutation request lifecycle so
			// (If-Match check → handler → store flush) is atomic per
			// app. Acquire BEFORE the If-Match-presence check so that
			// unconditional writes (missing If-Match) also serialise
			// per app — otherwise two concurrent unconditional DELETEs
			// on the same app would race, reintroducing the TOCTOU
			// gap this middleware exists to close.
			if s.store != nil {
				appLock := s.store.Locks().AppLock(id)
				appLock.Lock()
				defer appLock.Unlock()
			}

			ifMatch := strings.TrimSpace(r.Header.Get("If-Match"))
			if ifMatch == "" {
				// Spec promises this Warning on every unconditional
				// write — emit it before any store/meta short-circuit.
				// Use Add not Set: per RFC 7234 §5.5 Warning is a
				// list-valued header, so a future deprecation or
				// upstream-injected warning would be clobbered if we
				// Set here.
				w.Header().Add("Warning", `299 - "unconditional-write"`)
				next.ServeHTTP(w, r)
				return
			}
			// Caller provided If-Match. Without a store or persisted
			// metadata there's no rv to compare against — pass
			// through and let the downstream handler decide (it will
			// surface 404 if the resource doesn't exist).
			if s.store == nil {
				next.ServeHTTP(w, r)
				return
			}

			meta, ok := s.store.Meta(id)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			currentRV := strconv.FormatUint(meta.ResourceVersion, 10)
			// Strip optional double-quotes per RFC 7232 ETag syntax
			// (`"123"`); creekd's rv is an opaque string but tolerate
			// both quoted + unquoted client forms.
			ifMatch = strings.Trim(ifMatch, `"`)
			if ifMatch != currentRV {
				writePreconditionFailed(w, currentRV, ifMatch)
				return
			}
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
		// POST /v1/apps/{id}/deploy and /rollback are both spec
		// mutations (each creates a Release that flips the Active
		// pointer). Restart and reset are operations, not spec
		// writes — left out.
		if extractAppID(path) == "" {
			return false
		}
		return strings.HasSuffix(path, "/deploy") || strings.HasSuffix(path, "/rollback")
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
