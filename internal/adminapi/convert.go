package adminapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/runtime"
	"github.com/solcreek/creekd/internal/sandbox"
	"github.com/solcreek/creekd/internal/state"
	"github.com/solcreek/creekd/internal/supervisor"
)

// spawnToConfig converts a spec SpawnRequest to internal supervisor.Config.
func spawnToConfig(req apitypes.SpawnRequest) (supervisor.Config, error) {
	rt, err := parseRuntimePtr(req.Runtime)
	if err != nil {
		return supervisor.Config{}, err
	}
	return supervisor.Config{
		ID:              req.Id,
		Runtime:         rt,
		Entry:           derefStr(req.Entry),
		Command:         derefStr(req.Command),
		Args:            derefStrSlice(req.Args),
		Port:            req.Port,
		Env:             derefStrSlice(req.Env),
		CgroupLimits:    limitsToInternal(req.Limits),
		NetIsolation:    derefBool(req.NetIsolation),
		Sandbox:         sandboxToInternal(req.Sandbox),
		HealthCheckPath: derefStr(req.HealthCheckPath),
		VolumeMounts:    volumeMountsToInternal(req.VolumeMounts),
	}, nil
}

// deployToConfig converts a spec DeployRequest + app ID to internal deploy types.
func deployToConfig(id string, req apitypes.DeployRequest) (supervisor.DeployConfig, error) {
	rt, err := parseRuntimePtr(req.Runtime)
	if err != nil {
		return supervisor.DeployConfig{}, err
	}
	return supervisor.DeployConfig{
		Config: supervisor.Config{
			ID:              id,
			Runtime:         rt,
			Entry:           derefStr(req.Entry),
			Command:         derefStr(req.Command),
			Args:            derefStrSlice(req.Args),
			Port:            req.Port,
			Env:             derefStrSlice(req.Env),
			CgroupLimits:    limitsToInternal(req.Limits),
			NetIsolation:    derefBool(req.NetIsolation),
			Sandbox:         sandboxToInternal(req.Sandbox),
			HealthCheckPath: derefStr(req.HealthCheckPath),
			VolumeMounts:    volumeMountsToInternal(req.VolumeMounts),
		},
		ReadyTimeout:      msOrDuration(req.ReadyTimeoutMs, 0),
		PollInterval:      msOrDuration(req.PollIntervalMs, 0),
		GracefulV1Timeout: msOrDuration(req.GracefulV1Ms, 0),
	}, nil
}

// appToView converts a supervisor App to the spec AppView.
func appToView(app *supervisor.App) apitypes.AppView {
	if app == nil {
		return apitypes.AppView{}
	}
	v := apitypes.AppView{
		Id:             app.ID,
		Command:        app.Command,
		Port:           app.Port,
		Status:         apitypes.AppViewStatus(app.Status().String()),
		Pid:            app.PID(),
		UptimeMs:       app.Uptime().Milliseconds(),
		RestartCount:   app.RestartCount(),
		HealthFailures: app.HealthFailures(),
	}
	if rt := string(app.Runtime); rt != "" {
		r := apitypes.Runtime(rt)
		v.Runtime = &r
	}
	if args := append([]string(nil), app.Args...); len(args) > 0 {
		v.Args = &args
	}
	if env := app.Env(); len(env) > 0 {
		v.Env = &env
	}
	if app.NetIP != nil {
		s := app.NetIP.String()
		v.NetIp = &s
	}
	return v
}

// appToEnvelope converts a runtime supervisor.App + its persisted
// state.AppMetadata into the k8s-style envelope (apitypes.App). Used
// by GetApp as the first endpoint to expose the envelope shape.
//
// Conditions are recomputed from supervisor runtime state via the
// caller-supplied tracker so LastTransitionTime reflects the real
// moment of the last status flip across calls. Tracker may be nil
// in tests that don't care about LTT continuity; in that case a
// fresh tracker is constructed per call (LTT = now).
//
// observedGeneration is wired through but not yet asynchronous;
// see #10 for the dedicated async writer that will eventually
// populate it from the deploy flow rather than mirroring
// meta.Generation.
func appToEnvelope(app *supervisor.App, meta state.AppMetadata, ct *conditionTracker) apitypes.App {
	if app == nil {
		return apitypes.App{}
	}
	// meta.UID is a UUIDv7 string written by state package; parse
	// back into the wire-format UUID type. uuid.Parse error here
	// means state was corrupted — we surface a zero UID rather than
	// panic in a handler.
	parsedUID, _ := uuid.Parse(meta.UID)

	if ct == nil {
		ct = newConditionTracker()
	}
	observedGen := meta.Generation // see comment above; #10 will fix
	conditions := ct.computeAppConditions(app.ID, app.Status(), meta.Generation, observedGen, time.Now().UTC())

	envelope := apitypes.App{
		ApiVersion: apitypes.CreekDevv1alpha1,
		Kind:       apitypes.AppKindApp,
		Metadata: apitypes.AppMetadata{
			Name:              app.ID,
			Uid:               parsedUID,
			Generation:        meta.Generation,
			ResourceVersion:   strconv.FormatUint(meta.ResourceVersion, 10),
			CreationTimestamp: meta.CreationTimestamp,
		},
		Spec: apitypes.AppSpec{},
		Status: apitypes.AppStatus{
			ObservedGeneration: observedGen,
			Conditions:         conditions,
			CurrentPid:         app.PID(),
			CurrentPort:        app.Port,
			RestartCount:       app.RestartCount(),
			HealthFailures:     app.HealthFailures(),
			UptimeMs:           app.Uptime().Milliseconds(),
		},
	}
	envelope.Spec.Port = ptr(app.Port)
	envelope.Spec.Command = ptr(app.Command)
	if rt := string(app.Runtime); rt != "" {
		envelope.Spec.Runtime = &rt
	}
	if args := append([]string(nil), app.Args...); len(args) > 0 {
		envelope.Spec.Args = &args
	}
	if env := app.Env(); len(env) > 0 {
		envelope.Spec.Env = &env
	}
	if app.NetIP != nil {
		s := app.NetIP.String()
		envelope.Status.NetIp = &s
	}
	return envelope
}

// resolveOriginalArtifactRelease returns the ReleaseSeq of the
// first-appearance artifact for a chain of rollbacks. If target
// is itself a rollback record (Spec.OriginalArtifactRelease set),
// the chain pointer is propagated; otherwise the target IS the
// first appearance. The new release created by rolling back to
// target inherits this value so `creek releases` audit walks
// don't re-resolve at display time.
func resolveOriginalArtifactRelease(target state.Release) int64 {
	if target.Spec.OriginalArtifactRelease != 0 {
		return target.Spec.OriginalArtifactRelease
	}
	return target.Spec.ReleaseSeq
}

// releaseToWire converts a persisted state.Release to the wire
// shape (apitypes.Release). ConfigSnapshot is intentionally NOT
// returned on the wire — it's an internal rollback enabler that
// would expose env-var values to anyone reading the release
// ledger over the admin API. Restoring the snapshot is a
// server-internal concern.
func releaseToWire(r state.Release) apitypes.Release {
	uid, _ := uuid.Parse(r.UID)
	return apitypes.Release{
		Uid:               uid,
		Phase:             apitypes.ReleasePhase(r.Phase),
		CreationTimestamp: r.CreationTimestamp,
		Spec:              releaseSpecToWire(r.Spec),
	}
}

func releaseSpecToWire(s state.ReleaseSpec) apitypes.ReleaseSpec {
	appUID, _ := uuid.Parse(s.AppUID)
	spec := apitypes.ReleaseSpec{
		AppUid:     appUID,
		ReleaseSeq: s.ReleaseSeq,
	}
	if s.GitSha != "" {
		spec.GitSha = &s.GitSha
	}
	if s.Image != "" {
		spec.Image = &s.Image
	}
	if s.EnvHash != "" {
		spec.EnvHash = &s.EnvHash
	}
	if s.CreatedBy != "" {
		spec.CreatedBy = &s.CreatedBy
	}
	if s.RolledBackFrom != 0 {
		v := s.RolledBackFrom
		spec.RolledBackFrom = &v
	}
	if s.OriginalArtifactRelease != 0 {
		v := s.OriginalArtifactRelease
		spec.OriginalArtifactRelease = &v
	}
	return spec
}

// releaseActor returns a stable identifier for the caller of a
// release-creating request: the Bearer token's sha256 prefix when
// auth is on, falling back to the source IP. Matches the audit
// logger's hashToken convention so cross-referencing audit.log
// against the release ledger is straightforward.
func releaseActor(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return hashToken(strings.TrimPrefix(h, "Bearer "))
	}
	return r.RemoteAddr
}

// volumeToView converts a supervisor Volume to the spec VolumeView.
func volumeToView(v supervisor.Volume) apitypes.VolumeView {
	view := apitypes.VolumeView{
		Id:          v.ID,
		BackingPath: v.BackingPath,
	}
	if v.ReadOnly {
		view.ReadOnly = &v.ReadOnly
	}
	if v.FSType != "" {
		view.FsType = &v.FSType
	}
	return view
}

// --- helpers ---

func parseRuntimePtr(r *apitypes.Runtime) (runtime.Runtime, error) {
	if r == nil {
		return "", nil
	}
	return runtime.Parse(string(*r))
}

func limitsToInternal(l *apitypes.Limits) *cgroup.Limits {
	if l == nil {
		return nil
	}
	out := cgroup.Limits{
		MemoryHigh: derefInt64(l.MemoryHighBytes),
		MemoryMax:  derefInt64(l.MemoryMaxBytes),
		PidsMax:    derefInt64(l.PidsMax),
		CPUQuota:   derefInt64(l.CpuQuotaUs),
		CPUPeriod:  derefInt64(l.CpuPeriodUs),
	}
	if out.MemoryHigh == 0 && out.MemoryMax == 0 && out.PidsMax == 0 && out.CPUQuota == 0 {
		return nil
	}
	return &out
}

func sandboxToInternal(s *apitypes.Sandbox) *sandbox.Spec {
	if s == nil {
		return nil
	}
	spec := sandbox.Spec{
		PIDNamespace:   derefBool(s.PidNamespace),
		UTSNamespace:   derefBool(s.UtsNamespace),
		IPCNamespace:   derefBool(s.IpcNamespace),
		MountNamespace: derefBool(s.MountNamespace),
		UserNamespace:  derefBool(s.UserNamespace),
		NoNewPrivs:     derefBool(s.NoNewPrivs),
		Chroot:         derefStr(s.Chroot),
	}
	if !spec.Any() {
		return nil
	}
	return &spec
}

func volumeMountsToInternal(vms *[]apitypes.VolumeMount) []supervisor.VolumeMount {
	if vms == nil || len(*vms) == 0 {
		return nil
	}
	out := make([]supervisor.VolumeMount, len(*vms))
	for i, v := range *vms {
		out[i] = supervisor.VolumeMount{
			VolumeID: v.VolumeId,
			SubPath:  derefStr(v.SubPath),
			Target:   v.Target,
			ReadOnly: derefBool(v.ReadOnly),
		}
	}
	return out
}

func msOrDuration(ms *int64, fallback time.Duration) time.Duration {
	if ms == nil || *ms <= 0 {
		return fallback
	}
	return time.Duration(*ms) * time.Millisecond
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefStrSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

func ptr[T any](v T) *T {
	return &v
}
