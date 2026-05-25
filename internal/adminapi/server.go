package adminapi

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/dispatch"
	"github.com/solcreek/creekd/internal/state"
	"github.com/solcreek/creekd/internal/supervisor"
)

// Compile-time check that Server implements the generated interface.
var _ apitypes.ServerInterface = (*Server)(nil)

// Server is the HTTP/JSON admin handler. Construct with New; obtain
// the http.Handler via Handler.
type Server struct {
	sup        *supervisor.Supervisor
	router     *dispatch.Router
	token      string
	store      *state.Store
	audit      *AuditLogger
	conditions *conditionTracker

	mux *http.ServeMux
}

// SetStore wires a persistence Store into the server.
func (s *Server) SetStore(st *state.Store) { s.store = st }

// SetAuditLogger enables structured audit logging for all mutating
// API operations.
func (s *Server) SetAuditLogger(a *AuditLogger) { s.audit = a }

// New returns a Server backed by sup.
func New(sup *supervisor.Supervisor, dispatchRouter *dispatch.Router, token string) *Server {
	s := &Server{
		sup:        sup,
		router:     dispatchRouter,
		token:      token,
		conditions: newConditionTracker(),
		mux:        http.NewServeMux(),
	}
	// Wire the generated router. Middleware execution order on the
	// inbound path: audit → auth → cas → handler. Audit wraps the
	// whole stack so it observes the final status (incl. 401 / 412);
	// auth rejects unauthenticated requests before any state lookup;
	// cas validates If-Match only after the caller is authenticated,
	// so 412 responses don't leak resourceVersion to anonymous probes.
	//
	// oapi-codegen wraps in slice order — slice[0] becomes innermost,
	// slice[len-1] becomes outermost. Execution = reversed slice. So
	// to get audit→auth→cas→handler we list them in reverse:
	// [cas, auth, audit].
	apiHandler := apitypes.HandlerWithOptions(s, apitypes.StdHTTPServerOptions{
		BaseRouter:       s.mux,
		Middlewares:      []apitypes.MiddlewareFunc{s.casMiddleware(), s.authMiddleware(), s.auditMiddleware()},
		ErrorHandlerFunc: s.handleParamError,
	})
	_ = apiHandler // routes registered on s.mux
	return s
}

// Handler returns the configured http.Handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// EnablePprof mounts net/http/pprof handlers gated by bearer token.
func (s *Server) EnablePprof() {
	s.mux.HandleFunc("GET /debug/pprof/", s.guardFunc(pprof.Index))
	s.mux.HandleFunc("GET /debug/pprof/cmdline", s.guardFunc(pprof.Cmdline))
	s.mux.HandleFunc("GET /debug/pprof/profile", s.guardFunc(pprof.Profile))
	s.mux.HandleFunc("GET /debug/pprof/symbol", s.guardFunc(pprof.Symbol))
	s.mux.HandleFunc("GET /debug/pprof/trace", s.guardFunc(pprof.Trace))
}

// SetMetricsHandler mounts a Prometheus-format /metrics endpoint.
func (s *Server) SetMetricsHandler(h http.Handler) {
	if h == nil {
		return
	}
	s.mux.Handle("GET /metrics", s.guardFunc(h.ServeHTTP))
}

// --- Middleware ---

// authMiddleware returns a MiddlewareFunc that checks the bearer token.
func (s *Server) authMiddleware() apitypes.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.token != "" && !s.checkBearer(r) {
				writeError(w, http.StatusUnauthorized, string(apitypes.ErrorCodeUnauthorized),
					"missing or invalid bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// auditMiddleware returns a MiddlewareFunc that logs mutating operations.
func (s *Server) auditMiddleware() apitypes.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if s.audit == nil || !isMutating(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			start := time.Now()
			aw := &auditResponseWriter{ResponseWriter: w, statusCode: 200}
			next.ServeHTTP(aw, r)
			s.audit.Log(AuditEntry{
				Timestamp:  start.UTC().Format(time.RFC3339),
				Method:     r.Method,
				Path:       r.URL.Path,
				AppID:      extractAppID(r.URL.Path),
				Action:     actionFromRequest(r.Method, r.URL.Path),
				Actor:      hashToken(s.extractToken(r)),
				StatusCode: aw.statusCode,
				DurationMS: time.Since(start).Milliseconds(),
				SourceIP:   r.RemoteAddr,
			})
		})
	}
}

// handleParamError handles parameter validation errors from the
// generated router. The wrapper parses path / header / query params
// BEFORE the middleware chain runs, so a parse error (malformed id,
// duplicated If-Match header, etc.) reaches us here without auth or
// audit having had a chance to act. Mirror what those middlewares
// would have done: enforce auth so anonymous probers get a 401 (not
// a 400 that leaks endpoint existence), and emit an audit entry so
// the attempt is logged for mutating verbs.
func (s *Server) handleParamError(w http.ResponseWriter, r *http.Request, err error) {
	if s.token != "" && !s.checkBearer(r) {
		s.emitAuditOnEarlyError(r, http.StatusUnauthorized)
		writeError(w, http.StatusUnauthorized, string(apitypes.ErrorCodeUnauthorized),
			"missing or invalid bearer token")
		return
	}
	s.emitAuditOnEarlyError(r, http.StatusBadRequest)
	writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
}

// emitAuditOnEarlyError logs a request that failed wrapper-level
// validation before the audit middleware ran. No-op when audit is not
// configured or the verb is non-mutating (read-only param errors
// don't warrant an audit entry).
func (s *Server) emitAuditOnEarlyError(r *http.Request, statusCode int) {
	if s.audit == nil || !isMutating(r.Method) {
		return
	}
	s.audit.Log(AuditEntry{
		Timestamp:  time.Now().UTC().Format(time.RFC3339),
		Method:     r.Method,
		Path:       r.URL.Path,
		AppID:      extractAppID(r.URL.Path),
		Action:     actionFromRequest(r.Method, r.URL.Path),
		Actor:      hashToken(s.extractToken(r)),
		StatusCode: statusCode,
		DurationMS: 0,
		SourceIP:   r.RemoteAddr,
	})
}

// guardFunc wraps h with bearer-token check (for pprof/metrics, not API routes).
func (s *Server) guardFunc(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" && !s.checkBearer(r) {
			writeError(w, http.StatusUnauthorized, string(apitypes.ErrorCodeUnauthorized),
				"missing or invalid bearer token")
			return
		}
		h(w, r)
	}
}

func (s *Server) extractToken(r *http.Request) string {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if strings.HasPrefix(got, prefix) {
		return got[len(prefix):]
	}
	return ""
}

func (s *Server) checkBearer(r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	provided := got[len(prefix):]
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.token)) == 1
}

// --- ServerInterface implementation ---

func (s *Server) SpawnApp(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SpawnRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		return
	}
	if err := supervisor.ValidateID(req.Id); err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		return
	}
	if req.Port == 0 {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), "port is required")
		return
	}

	cfg, err := spawnToConfig(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		return
	}

	app, err := s.sup.Spawn(cfg)
	if err != nil {
		s.mapSpawnError(w, err)
		return
	}

	if s.router != nil {
		host := ""
		if app.NetIP != nil {
			host = app.NetIP.String()
		}
		if rerr := s.router.SetAddr(req.Id, host, req.Port); rerr != nil {
			_ = s.sup.Stop(req.Id)
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest),
				"dispatch.SetAddr: "+rerr.Error())
			return
		}
	}

	if s.store != nil {
		if serr := s.store.AddApp(cfg); serr != nil {
			_ = s.sup.Stop(req.Id)
			if s.router != nil {
				s.router.Remove(req.Id)
			}
			writeStoreError(w, "state.AddApp", serr)
			return
		}
	}

	writeJSON(w, http.StatusCreated, appToView(app))
}

func (s *Server) mapSpawnError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, supervisor.ErrAlreadyRunning):
		writeError(w, http.StatusConflict, string(apitypes.ErrorCodeAlreadyRunning), err.Error())
	case errors.Is(err, supervisor.ErrInvalidID):
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeInvalidId), err.Error())
	default:
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
	}
}

func (s *Server) ListApps(w http.ResponseWriter, _ *http.Request) {
	apps := s.sup.List()
	views := make([]apitypes.AppView, 0, len(apps))
	for _, a := range apps {
		views = append(views, appToView(a))
	}
	writeJSON(w, http.StatusOK, apitypes.ListAppsResponse{Apps: views})
}

func (s *Server) GetApp(w http.ResponseWriter, _ *http.Request, id apitypes.AppID) {
	app := s.sup.Get(id)
	if app == nil {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
		return
	}
	// Envelope refactor lands per-handler. GetApp is the first
	// endpoint to expose the k8s-style {apiVersion, kind, metadata,
	// spec, status} shape. Other handlers still return AppView until
	// they're refactored individually.
	//
	// If store is not configured (some test paths) or the app
	// predates the metadata era (in-memory-only spawn), fall back to
	// a zero-metadata envelope rather than 500ing — the runtime
	// state is still authoritative for status fields.
	var meta state.AppMetadata
	if s.store != nil {
		if m, ok := s.store.Meta(id); ok {
			meta = m
		}
	}
	writeJSON(w, http.StatusOK, appToEnvelope(app, meta, s.conditions))
}

func (s *Server) StopApp(w http.ResponseWriter, _ *http.Request, id apitypes.AppID, _ apitypes.StopAppParams) {
	if err := s.sup.Stop(id); err != nil {
		if errors.Is(err, supervisor.ErrNotFound) {
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
			return
		}
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), err.Error())
		return
	}
	if s.router != nil {
		s.router.Remove(id)
	}
	if s.store != nil {
		if err := s.store.RemoveApp(id); err != nil {
			writeStoreError(w, "state.RemoveApp", err)
			return
		}
	}
	if s.conditions != nil {
		s.conditions.forget(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) DeployApp(w http.ResponseWriter, r *http.Request, id apitypes.AppID, _ apitypes.DeployAppParams) {
	var req apitypes.DeployRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		return
	}
	if req.Port == 0 {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), "port is required")
		return
	}

	dcfg, err := deployToConfig(id, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		return
	}

	app, err := s.sup.Deploy(r.Context(), s.router, dcfg)
	if err != nil {
		switch {
		case errors.Is(err, supervisor.ErrNotFound):
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), err.Error())
		case errors.Is(err, supervisor.ErrPortConflict):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodePortConflict), err.Error())
		case errors.Is(err, supervisor.ErrDeployUnhealthy):
			writeError(w, http.StatusBadGateway, string(apitypes.ErrorCodeDeployUnhealthy), err.Error())
		case errors.Is(err, supervisor.ErrDeployConflict):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodeConflict), err.Error())
		default:
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		}
		return
	}
	if s.store != nil {
		if serr := s.store.AddApp(dcfg.Config); serr != nil {
			writeStoreError(w, "state.AddApp (deploy)", serr)
			return
		}
		// Append a Release record to the ledger. The deploy succeeded
		// at the supervisor layer; this captures the artifact + env
		// snapshot so a future rollback can re-run the exact config.
		// CreateRelease bumps ResourceVersion (status mutation) on
		// the same WAL pending — at-most-one-Active is preserved
		// across crash.
		cfgCopy := supervisor.CloneConfig(dcfg.Config)
		spec := state.ReleaseSpec{
			EnvHash:        state.EnvHash(cfgCopy.Env),
			CreatedBy:      releaseActor(r),
			ConfigSnapshot: &cfgCopy,
		}
		if _, rerr := s.store.CreateRelease(id, state.ReleaseInput{Spec: spec}); rerr != nil {
			writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
				"state.CreateRelease (deploy): "+rerr.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, appToView(app))
}

// RollbackApp re-runs the supervisor against the ConfigSnapshot of
// the target Release (identified by `?to=<seq>`) and appends a new
// Release record carrying RolledBackFrom + OriginalArtifactRelease
// pointers. The prior Active release flips to RolledBack (distinct
// from Superseded — distinguishes "rolled away from" vs "replaced
// by newer deploy").
//
// 0.0.x scope:
//   - Env: rollback restores the env that was captured in the
//     target's ConfigSnapshot. The Heroku-style env_rollback_conflict
//     guard for structurally-diverged env-var KEY sets ships in
//     0.1.0 alongside KEK envelope encryption.
//   - Image registry: not present in 0.0.x; ConfigSnapshot IS the
//     artifact. A target Release missing ConfigSnapshot returns 404
//     release_artifact_pruned — the ledger entry has nothing to
//     re-run.
func (s *Server) RollbackApp(w http.ResponseWriter, r *http.Request, id apitypes.AppID, params apitypes.RollbackAppParams) {
	if s.store == nil {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
			"rollback requires a store; daemon not configured for persistence")
		return
	}
	if params.To <= 0 {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest),
			"?to=<releaseSeq> required (positive integer)")
		return
	}

	target, ok := s.store.FindRelease(id, params.To)
	if !ok {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeReleaseArtifactPruned),
			fmt.Sprintf("release %d not found in app %q ledger", params.To, id))
		return
	}
	if target.Spec.ConfigSnapshot == nil {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeReleaseArtifactPruned),
			fmt.Sprintf("release %d has no config snapshot — cannot re-run", params.To))
		return
	}

	// Re-run the snapshotted config through the same deploy path so
	// the supervisor swaps process trees + dispatch entries atomically.
	dcfg := supervisor.DeployConfig{Config: supervisor.CloneConfig(*target.Spec.ConfigSnapshot)}
	dcfg.Config.ID = id
	if _, err := s.sup.Deploy(r.Context(), s.router, dcfg); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrNotFound):
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), err.Error())
		case errors.Is(err, supervisor.ErrPortConflict):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodePortConflict), err.Error())
		case errors.Is(err, supervisor.ErrDeployUnhealthy):
			writeError(w, http.StatusBadGateway, string(apitypes.ErrorCodeDeployUnhealthy), err.Error())
		default:
			writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
				"sup.Deploy (rollback): "+err.Error())
		}
		return
	}
	if serr := s.store.AddApp(dcfg.Config); serr != nil {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
			"state.AddApp (rollback): "+serr.Error())
		return
	}

	original := resolveOriginalArtifactRelease(target)

	cfgCopy := supervisor.CloneConfig(dcfg.Config)
	newSpec := state.ReleaseSpec{
		GitSha:                  target.Spec.GitSha,
		Image:                   target.Spec.Image,
		EnvHash:                 state.EnvHash(cfgCopy.Env),
		CreatedBy:               releaseActor(r),
		RolledBackFrom:          target.Spec.ReleaseSeq,
		OriginalArtifactRelease: original,
		ConfigSnapshot:          &cfgCopy,
	}
	newRel, rerr := s.store.CreateRelease(id, state.ReleaseInput{
		Spec:             newSpec,
		PriorActivePhase: state.ReleasePhaseRolledBack,
	})
	if rerr != nil {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
			"state.CreateRelease (rollback): "+rerr.Error())
		return
	}
	writeJSON(w, http.StatusOK, releaseToWire(newRel))
}

func (s *Server) RestartApp(w http.ResponseWriter, r *http.Request, id apitypes.AppID) {
	var req apitypes.RestartRequest
	if r.ContentLength > 0 || r.Body != http.NoBody {
		if err := decodeJSONAllowEmpty(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
			return
		}
	}

	app, err := s.sup.Restart(id, msOrDuration(req.TimeoutMs, 0))
	if err != nil {
		if errors.Is(err, supervisor.ErrNotFound) {
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, appToView(app))
}

func (s *Server) ResetApp(w http.ResponseWriter, _ *http.Request, id apitypes.AppID) {
	if err := s.sup.Reset(id); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrNotFound):
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
		case errors.Is(err, supervisor.ErrNotCrashLooping):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodeConflict), err.Error())
		default:
			writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), err.Error())
		}
		return
	}
	if app := s.sup.Get(id); app != nil {
		writeJSON(w, http.StatusOK, appToView(app))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) GetAppEvents(w http.ResponseWriter, r *http.Request, id apitypes.AppID) {
	app := s.sup.Get(id)
	if app == nil {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	subID, ch := s.sup.Events.Subscribe(64)
	defer s.sup.Events.Unsubscribe(subID)

	ctx := r.Context()
	enc := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.AppID != id {
				continue
			}
			fmt.Fprintf(w, "data: ")
			enc.Encode(evt)
			flusher.Flush()
		}
	}
}

func (s *Server) GetAppStats(w http.ResponseWriter, _ *http.Request, id apitypes.AppID) {
	app := s.sup.Get(id)
	if app == nil {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
		return
	}
	v := apitypes.StatsView{Id: id}
	cg := app.Cgroup()
	if cg == nil {
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.CgroupEnabled = true

	var firstErr error
	if cur, err := cg.MemoryCurrent(); err == nil {
		v.MemoryCurrentBytes = &cur
	} else if firstErr == nil {
		firstErr = err
	}
	if max, err := cg.MemoryMax(); err == nil {
		v.MemoryMaxBytes = &max
	} else if firstErr == nil {
		firstErr = err
	}
	if pc, err := cg.PidsCurrent(); err == nil {
		v.PidsCurrent = &pc
	} else if firstErr == nil {
		firstErr = err
	}
	if usec, err := cg.CPUUsageMicros(); err == nil {
		v.CpuUsageUsec = &usec
	} else if firstErr == nil {
		firstErr = err
	}
	if st, err := cg.Stats(); err == nil {
		v.OomKills = &st.OOMKill
	} else if firstErr == nil {
		firstErr = err
	}
	if firstErr != nil {
		s := firstErr.Error()
		v.ReadErr = &s
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) GetAppLogs(w http.ResponseWriter, r *http.Request, id apitypes.AppID, params apitypes.GetAppLogsParams) {
	if app := s.sup.Get(id); app == nil {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "app not found")
		return
	}

	logPath := s.sup.AppLogPath(id)
	if logPath == "" {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest),
			"log capture disabled (set Supervisor.LogDir)")
		return
	}

	tail := defaultTail
	if params.Tail != nil {
		if *params.Tail < 0 {
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), "tail: must be >= 0")
			return
		}
		if *params.Tail > maxTail {
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest),
				fmt.Sprintf("tail: must be <= %d", maxTail))
			return
		}
		tail = *params.Tail
	}

	follow := params.Follow != nil && *params.Follow == apitypes.True

	if follow {
		streamLogs(w, r, logPath, tail)
		return
	}
	tailLogs(w, logPath, tail)
}

func (s *Server) RegisterVolume(w http.ResponseWriter, r *http.Request) {
	var req apitypes.VolumeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), "decode: "+err.Error())
		return
	}
	v := supervisor.Volume{
		ID:          req.Id,
		BackingPath: req.BackingPath,
		ReadOnly:    derefBool(req.ReadOnly),
	}
	if err := s.sup.RegisterVolume(v); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrVolumeAlreadyExists):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodeConflict), err.Error())
		case errors.Is(err, supervisor.ErrVolumeBackingMissing),
			errors.Is(err, supervisor.ErrVolumeRootRequired):
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		default:
			writeError(w, http.StatusBadRequest, string(apitypes.ErrorCodeBadRequest), err.Error())
		}
		return
	}
	stored, _ := s.sup.Volume(req.Id)

	if s.store != nil {
		if serr := s.store.AddVolume(stored); serr != nil {
			_ = s.sup.UnregisterVolume(req.Id, true)
			writeStoreError(w, "state.AddVolume", serr)
			return
		}
	}

	writeJSON(w, http.StatusCreated, volumeToView(stored))
}

func (s *Server) ListVolumes(w http.ResponseWriter, _ *http.Request) {
	vols := s.sup.Volumes()
	out := apitypes.ListVolumesResponse{Volumes: make([]apitypes.VolumeView, 0, len(vols))}
	for _, v := range vols {
		out.Volumes = append(out.Volumes, volumeToView(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) GetVolume(w http.ResponseWriter, _ *http.Request, id apitypes.VolumeID) {
	v, ok := s.sup.Volume(id)
	if !ok {
		writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), "volume not found")
		return
	}
	writeJSON(w, http.StatusOK, volumeToView(v))
}

func (s *Server) DeleteVolume(w http.ResponseWriter, _ *http.Request, id apitypes.VolumeID, params apitypes.DeleteVolumeParams) {
	force := params.Force != nil && *params.Force

	if err := s.sup.UnregisterVolume(id, force); err != nil {
		switch {
		case errors.Is(err, supervisor.ErrVolumeNotFound):
			writeError(w, http.StatusNotFound, string(apitypes.ErrorCodeNotFound), err.Error())
		case errors.Is(err, supervisor.ErrVolumeInUse):
			writeError(w, http.StatusConflict, string(apitypes.ErrorCodeConflict), err.Error())
		default:
			writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal), err.Error())
		}
		return
	}

	if s.store != nil {
		if err := s.store.RemoveVolume(id); err != nil {
			writeStoreError(w, "state.RemoveVolume", err)
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// --- shared helpers ---

const MaxRequestBodyBytes = 64 << 10

func decodeJSON(r *http.Request, dst any) error {
	return decodeJSONInner(nil, r, dst, false)
}

func decodeJSONAllowEmpty(r *http.Request, dst any) error {
	return decodeJSONInner(nil, r, dst, true)
}

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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apitypes.ErrorResponse{Code: apitypes.ErrorCode(code), Error: msg})
}

// writeStoreError maps a state.Store mutation error to the right HTTP
// status + ErrorCode. StorageCorruptedError surfaces as 503 with
// `storage_corrupted` so clients know the daemon's persistence is in
// a refusing-further-writes state. Anything else collapses to the
// existing 500 / `internal` shape.
//
// UnsupportedFilesystemError is NOT handled here — it's raised by
// NewStore at daemon startup, never at request time. The spec keeps
// the code in the enum for completeness (any client doing schema
// codegen will see both errors the daemon can produce); request-time
// callers only ever see storage_corrupted.
func writeStoreError(w http.ResponseWriter, prefix string, err error) {
	var corrupt *state.StorageCorruptedError
	if errors.As(err, &corrupt) {
		writeError(w, http.StatusServiceUnavailable, string(apitypes.ErrorCodeStorageCorrupted),
			prefix+": "+err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, string(apitypes.ErrorCodeInternal),
		prefix+": "+err.Error())
}
