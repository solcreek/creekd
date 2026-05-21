package adminapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/state"
	"github.com/solcreek/creekd/internal/supervisor"
)

// Server is the HTTP/JSON admin handler. Construct with New; obtain
// the http.Handler via Handler.
type Server struct {
	sup    *supervisor.Supervisor
	router *dispatch.Router
	token  string
	store  *state.Store // optional; set via SetStore to enable persistence

	mux *http.ServeMux
}

// SetStore wires a persistence Store into the server. Every
// successful Spawn / Deploy / Stop call writes through the store so
// the daemon's restart logic can restore the same app set. nil
// disables persistence (the default; only safe for ephemeral test
// runs).
func (s *Server) SetStore(st *state.Store) { s.store = st }

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

// EnablePprof mounts net/http/pprof's standard handlers under
// /debug/pprof/, gated by the server's bearer-token guard. Opt-in
// because pprof endpoints leak detailed memory contents and CPU
// profiling traces — exposing them unconditionally on a public
// listener would be a footgun. The main binary wires this in
// response to CREEKD_DEBUG_PPROF=1.
//
// Available routes (all GET):
//
//	/debug/pprof/         index — lists every profile
//	/debug/pprof/<name>   heap / goroutine / block / mutex / allocs
//	/debug/pprof/cmdline  process argv
//	/debug/pprof/profile  cpu profile (?seconds=N)
//	/debug/pprof/symbol   symbol resolution
//	/debug/pprof/trace    runtime trace (?seconds=N)
func (s *Server) EnablePprof() {
	s.mux.HandleFunc("GET /debug/pprof/", s.guard(pprof.Index))
	s.mux.HandleFunc("GET /debug/pprof/cmdline", s.guard(pprof.Cmdline))
	s.mux.HandleFunc("GET /debug/pprof/profile", s.guard(pprof.Profile))
	s.mux.HandleFunc("GET /debug/pprof/symbol", s.guard(pprof.Symbol))
	s.mux.HandleFunc("GET /debug/pprof/trace", s.guard(pprof.Trace))
}

// SetMetricsHandler mounts a Prometheus-format /metrics endpoint
// gated by the same bearer-token guard as the rest of the admin
// surface. Pass nil to leave it unmounted (the default). The handler
// is typically the output of internal/metrics.Metrics.Handler().
//
// Why guarded: per-app stats include process IDs, restart counts,
// memory pressure — operationally sensitive enough that we don't
// want an unauthenticated reader pulling them from a public address.
// Scraper-side: Prom config supports bearer tokens via the
// authorization stanza; OTel collector via the http_config block.
func (s *Server) SetMetricsHandler(h http.Handler) {
	if h == nil {
		return
	}
	s.mux.Handle("GET /metrics", s.guard(h.ServeHTTP))
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
	s.mux.HandleFunc("GET /v1/apps/{id}/stats", s.guard(s.handleStats))

	s.mux.HandleFunc("POST /v1/volumes", s.guard(s.handleVolumeRegister))
	s.mux.HandleFunc("GET /v1/volumes", s.guard(s.handleVolumesList))
	s.mux.HandleFunc("GET /v1/volumes/{id}", s.guard(s.handleVolumeGet))
	s.mux.HandleFunc("DELETE /v1/volumes/{id}", s.guard(s.handleVolumeDelete))
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
	if err := supervisor.ValidateID(req.ID); err != nil {
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

	cfg := supervisor.Config{
		ID:              req.ID,
		Runtime:         rt,
		Entry:           req.Entry,
		Command:         req.Command,
		Args:            req.Args,
		Port:            req.Port,
		Env:             req.Env,
		CgroupLimits:    req.Limits.toCgroupLimits(),
		NetIsolation:    req.NetIsolation,
		Sandbox:         req.Sandbox.toSandboxSpec(),
		HealthCheckPath: req.HealthCheckPath,
		VolumeMounts:    toSupervisorVolumeMounts(req.VolumeMounts),
	}

	app, err := s.sup.Spawn(cfg)
	if err != nil {
		s.mapSpawnError(w, err)
		return
	}

	if s.router != nil {
		// Route via the container IP when net isolation is on so
		// dispatch traffic crosses the bridge; otherwise fall back to
		// the default loopback host.
		host := ""
		if app.NetIP != nil {
			host = app.NetIP.String()
		}
		if rerr := s.router.SetAddr(req.ID, host, req.Port); rerr != nil {
			// Roll back the spawn — half-registered apps are worse
			// than a clean failure.
			_ = s.sup.Stop(req.ID)
			writeError(w, http.StatusBadRequest, CodeBadRequest,
				"dispatch.SetAddr: "+rerr.Error())
			return
		}
	}

	if s.store != nil {
		if serr := s.store.AddApp(cfg); serr != nil {
			// Persistence failure → roll back the in-memory state so
			// the operator sees a clean failure rather than a state
			// where the daemon restart would forget this app.
			_ = s.sup.Stop(req.ID)
			if s.router != nil {
				s.router.Remove(req.ID)
			}
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"state.AddApp: "+serr.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, viewOf(app))
}

// mapSpawnError translates supervisor.Spawn errors into HTTP codes.
func (s *Server) mapSpawnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, supervisor.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, CodeAlreadyRunning, err.Error())
	case errors.Is(err, supervisor.ErrInvalidID):
		writeError(w, http.StatusBadRequest, CodeInvalidID, err.Error())
	default:
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
	if s.store != nil {
		// Best-effort: a flush failure here means the next daemon
		// restart will re-spawn an app the operator just stopped.
		// Surfacing 500 to the operator gives them a chance to
		// retry. The process is already gone either way.
		if err := s.store.RemoveApp(id); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"state.RemoveApp: "+err.Error())
			return
		}
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
			ID:              id,
			Runtime:         rt,
			Entry:           req.Entry,
			Command:         req.Command,
			Args:            req.Args,
			Port:            req.Port,
			Env:             req.Env,
			CgroupLimits:    req.Limits.toCgroupLimits(),
			NetIsolation:    req.NetIsolation,
			Sandbox:         req.Sandbox.toSandboxSpec(),
			HealthCheckPath: req.HealthCheckPath,
			VolumeMounts:    toSupervisorVolumeMounts(req.VolumeMounts),
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
		case errors.Is(err, supervisor.ErrPortConflict):
			writeError(w, http.StatusConflict, CodePortConflict, err.Error())
		case errors.Is(err, supervisor.ErrDeployUnhealthy):
			writeError(w, http.StatusBadGateway, CodeUnhealthy, err.Error())
		case errors.Is(err, supervisor.ErrDeployConflict):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		}
		return
	}
	if s.store != nil {
		// Deploy replaces the previous config (different port, env,
		// runtime, etc). AddApp idempotently overwrites.
		if serr := s.store.AddApp(dcfg.Config); serr != nil {
			// Deploy has already swapped in-memory state to v2; we
			// cannot meaningfully roll back without re-deploying v1.
			// Log loudly via the response.
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"state.AddApp (deploy): "+serr.Error())
			return
		}
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

// handleStats handles GET /v1/apps/{id}/stats. Returns a StatsView
// with cgroup-tracked counters when the app was spawned with
// CgroupLimits; otherwise CgroupEnabled is false and the response
// carries only the ID + flag (caller can still read uptime /
// restart_count from the AppView returned by Get).
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	app := s.sup.Get(id)
	if app == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "app not found")
		return
	}
	v := StatsView{ID: id}
	cg := app.Cgroup()
	if cg == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.CgroupEnabled = true

	// Each read can fail independently (file missing during a
	// brief restart window, transient permission). Collect the
	// first error but keep going so the response surfaces whatever
	// was readable.
	var firstErr error
	if cur, err := cg.MemoryCurrent(); err == nil {
		v.MemoryCurrentBytes = cur
	} else if firstErr == nil {
		firstErr = err
	}
	if max, err := cg.MemoryMax(); err == nil {
		v.MemoryMaxBytes = max
	} else if firstErr == nil {
		firstErr = err
	}
	if pc, err := cg.PidsCurrent(); err == nil {
		v.PidsCurrent = pc
	} else if firstErr == nil {
		firstErr = err
	}
	if usec, err := cg.CPUUsageMicros(); err == nil {
		v.CPUUsageUsec = usec
	} else if firstErr == nil {
		firstErr = err
	}
	if st, err := cg.Stats(); err == nil {
		v.OOMKills = st.OOMKill
	} else if firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		v.ReadErr = firstErr.Error()
	}
	writeJSON(w, http.StatusOK, v)
}

// MaxRequestBodyBytes caps request body size at the decoder. Set to
// 64 KiB — well above any realistic SpawnRequest (~few KiB for
// command + args + env + volume_mounts) and far below what a single
// slow-body attacker can use to OOM the daemon. Pentest review (H4):
// without this cap, an attacker with admin token can stream a 10GiB
// body and OOM creekd.
const MaxRequestBodyBytes = 64 << 10

// decodeJSON reads and JSON-decodes the request body into dst.
// Rejects unknown fields so typos in client payloads surface as 400
// rather than silent drops. Body is capped at MaxRequestBodyBytes
// via http.MaxBytesReader so a slow / oversized body cannot OOM
// the daemon.
func decodeJSON(r *http.Request, dst any) error {
	return decodeJSONInner(nil, r, dst, false)
}

// decodeJSONAllowEmpty is like decodeJSON but tolerates an empty body
// — leaves dst at its zero value. Used by handlers (e.g. restart)
// where the body is optional.
func decodeJSONAllowEmpty(r *http.Request, dst any) error {
	return decodeJSONInner(nil, r, dst, true)
}

// decodeJSONInner is the shared implementation. w is optional; when
// non-nil http.MaxBytesReader wires the limit into the response
// hijacker for a clean 413 surface. When nil (most current call
// sites), the limit is still enforced — the caller just sees a
// generic decode error on overflow.
func decodeJSONInner(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) error {
	if r.Body == nil {
		if allowEmpty {
			return nil
		}
		return errors.New("empty body")
	}
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// handleVolumeRegister implements POST /v1/volumes.
//
// On success the supervisor has pinned an O_PATH fd of the backing
// directory under VolumeRoot and applied MS_PRIVATE propagation.
// The Volume is also persisted via state.Store so a daemon restart
// re-registers it before any app re-spawn can reference it.
func (s *Server) handleVolumeRegister(w http.ResponseWriter, r *http.Request) {
	var req VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "decode: "+err.Error())
		return
	}
	v := supervisor.Volume{
		ID:          req.ID,
		BackingPath: req.BackingPath,
		ReadOnly:    req.ReadOnly,
	}
	if err := s.sup.RegisterVolume(v); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrVolumeAlreadyExists):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		case errors.Is(err, supervisor.ErrVolumeBackingMissing),
			errors.Is(err, supervisor.ErrVolumeRootRequired):
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		default:
			writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		}
		return
	}
	stored, _ := s.sup.Volume(req.ID)

	if s.store != nil {
		if serr := s.store.AddVolume(stored); serr != nil {
			_ = s.sup.UnregisterVolume(req.ID, true)
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"state.AddVolume: "+serr.Error())
			return
		}
	}

	writeJSON(w, http.StatusCreated, volumeViewOf(stored))
}

// handleVolumesList implements GET /v1/volumes.
func (s *Server) handleVolumesList(w http.ResponseWriter, _ *http.Request) {
	vols := s.sup.Volumes()
	out := VolumesListResponse{Volumes: make([]VolumeView, 0, len(vols))}
	for _, v := range vols {
		out.Volumes = append(out.Volumes, volumeViewOf(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// handleVolumeGet implements GET /v1/volumes/{id}.
func (s *Server) handleVolumeGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, ok := s.sup.Volume(id)
	if !ok {
		writeError(w, http.StatusNotFound, CodeNotFound, "volume not found")
		return
	}
	writeJSON(w, http.StatusOK, volumeViewOf(v))
}

// handleVolumeDelete implements DELETE /v1/volumes/{id}. Refuses
// when any registered app still references the volume; pass
// ?force=true to override (the operator's "I know what I'm doing"
// switch — does NOT unmount existing binds, those persist by
// design).
func (s *Server) handleVolumeDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	force := r.URL.Query().Get("force") == "true"

	if err := s.sup.UnregisterVolume(id, force); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrVolumeNotFound):
			writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
		case errors.Is(err, supervisor.ErrVolumeInUse):
			writeError(w, http.StatusConflict, CodeConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, CodeInternal, err.Error())
		}
		return
	}

	if s.store != nil {
		if err := s.store.RemoveVolume(id); err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternal,
				"state.RemoveVolume: "+err.Error())
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
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
