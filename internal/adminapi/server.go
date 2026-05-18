package adminapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/supervisor"
)

// Server is the HTTP/JSON admin handler. Construct with New; obtain
// the http.Handler via Handler.
type Server struct {
	sup    *supervisor.Supervisor
	router *dispatch.Router
	token  string

	mux *http.ServeMux
}

// New returns a Server backed by sup. dispatchRouter, when non-nil,
// is updated on spawn/stop so that registered apps are reachable
// through the data-plane router. token, when non-empty, gates every
// endpoint behind an Authorization: Bearer <token> header.
func New(sup *supervisor.Supervisor, dispatchRouter *dispatch.Router, token string) *Server {
	s := &Server{
		sup:    sup,
		router: dispatchRouter,
		token:  token,
		mux:    http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler returns the configured http.Handler. Auth + JSON
// content-type are applied in middleware.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// routes wires URL patterns to handlers.
func (s *Server) routes() {
	// Go 1.22's pattern syntax lets us mix methods and path variables
	// in one pattern string.
	s.mux.HandleFunc("POST /v1/apps", s.guard(s.handleSpawn))
	s.mux.HandleFunc("GET /v1/apps", s.guard(s.handleList))
	s.mux.HandleFunc("GET /v1/apps/{id}", s.guard(s.handleGet))
	s.mux.HandleFunc("DELETE /v1/apps/{id}", s.guard(s.handleStop))
	s.mux.HandleFunc("POST /v1/apps/{id}/deploy", s.guard(s.handleDeploy))
	s.mux.HandleFunc("POST /v1/apps/{id}/reset", s.guard(s.handleReset))
	s.mux.HandleFunc("POST /v1/apps/{id}/restart", s.guard(s.handleRestart))
	s.mux.HandleFunc("GET /v1/apps/{id}/logs", s.guard(s.handleLogs))
}

// guard wraps h with the bearer-token check. When s.token is empty
// the guard is a passthrough.
func (s *Server) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			if !s.checkBearer(r) {
				writeError(w, http.StatusUnauthorized, CodeUnauthorized,
					"missing or invalid bearer token")
				return
			}
		}
		h(w, r)
	}
}

// checkBearer returns true if the Authorization header carries the
// configured bearer token. Uses a constant-time comparison.
func (s *Server) checkBearer(r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	provided := got[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

// handleSpawn handles POST /v1/apps.
func (s *Server) handleSpawn(w http.ResponseWriter, r *http.Request) {
	var req SpawnRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "id is required")
		return
	}
	if req.Port == 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "port is required")
		return
	}

	rt, err := parseRuntime(req.Runtime)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}

	cfg := supervisor.Config{
		ID:           req.ID,
		Runtime:      rt,
		Entry:        req.Entry,
		Command:      req.Command,
		Args:         req.Args,
		Port:         req.Port,
		Env:          req.Env,
		CgroupLimits: req.Limits.toCgroupLimits(),
	}

	app, err := s.sup.Spawn(cfg)
	if err != nil {
		s.mapSpawnError(w, err)
		return
	}

	if s.router != nil {
		if rerr := s.router.Set(req.ID, req.Port); rerr != nil {
			// Roll back the spawn — half-registered apps are worse
			// than a clean failure.
			_ = s.sup.Stop(req.ID)
			writeError(w, http.StatusBadRequest, CodeBadRequest,
				"dispatch.Set: "+rerr.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, viewOf(app))
}

// mapSpawnError translates supervisor.Spawn errors into HTTP codes.
func (s *Server) mapSpawnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, supervisor.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, CodeConflict, err.Error())
	default:
		// Spawn returns descriptive errors for empty fields, bad
		// runtime, missing binary, etc. — surface them as 400.
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	}
}

// handleList handles GET /v1/apps.
func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	apps := s.sup.List()
	views := make([]AppView, 0, len(apps))
	for _, a := range apps {
		views = append(views, viewOf(a))
	}
	writeJSON(w, http.StatusOK, ListResponse{Apps: views})
}

// handleGet handles GET /v1/apps/{id}.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app := s.sup.Get(id)
	if app == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "app not found")
		return
	}
	writeJSON(w, http.StatusOK, viewOf(app))
}

// handleStop handles DELETE /v1/apps/{id}.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sup.Stop(id); err != nil {
		if errors.Is(err, supervisor.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	if s.router != nil {
		s.router.Remove(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeploy handles POST /v1/apps/{id}/deploy.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req DeployRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	if req.Port == 0 {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "port is required")
		return
	}
	rt, err := parseRuntime(req.Runtime)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}

	dcfg := supervisor.DeployConfig{
		Config: supervisor.Config{
			ID:           id,
			Runtime:      rt,
			Entry:        req.Entry,
			Command:      req.Command,
			Args:         req.Args,
			Port:         req.Port,
			Env:          req.Env,
			CgroupLimits: req.Limits.toCgroupLimits(),
		},
		ReadyTimeout:      msOr(req.ReadyTimeoutMS, 0),
		PollInterval:      msOr(req.PollIntervalMS, 0),
		GracefulV1Timeout: msOr(req.GracefulV1MS, 0),
	}

	app, err := s.sup.Deploy(r.Context(), s.router, dcfg)
	if err != nil {
		switch {
		case errors.Is(err, supervisor.ErrNotFound):
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
		case errors.Is(err, supervisor.ErrDeployUnhealthy):
			writeError(w, http.StatusBadGateway, CodeUnhealthy, err.Error())
		case errors.Is(err, supervisor.ErrDeployConflict):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, viewOf(app))
}

// handleRestart handles POST /v1/apps/{id}/restart. Body is optional;
// an empty or "{}" body defaults timeout_ms to 0 (supervisor uses 10s).
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req RestartRequest
	if r.ContentLength > 0 || r.Body != http.NoBody {
		if err := decodeJSONAllowEmpty(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
			return
		}
	}

	app, err := s.sup.Restart(id, msOr(req.TimeoutMS, 0))
	if err != nil {
		if errors.Is(err, supervisor.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, viewOf(app))
}

// handleReset handles POST /v1/apps/{id}/reset.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sup.Reset(id); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrNotFound):
			writeError(w, http.StatusNotFound, CodeNotFound, "app not found")
		case errors.Is(err, supervisor.ErrNotCrashLooping):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		}
		return
	}
	if app := s.sup.Get(id); app != nil {
		writeJSON(w, http.StatusOK, viewOf(app))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSON reads and JSON-decodes the request body into dst.
// Rejects unknown fields so typos in client payloads surface as 400
// rather than silent drops.
func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

// decodeJSONAllowEmpty is like decodeJSON but tolerates an empty body
// — leaves dst at its zero value. Used by handlers (e.g. restart)
// where the body is optional.
func decodeJSONAllowEmpty(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// writeJSON writes status + JSON body.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError writes a JSON error response with the given status, code,
// and message.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorResponse{Code: code, Message: msg})
}
