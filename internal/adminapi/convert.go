package adminapi

import (
	"time"

	"github.com/solcreek/creekd/internal/apitypes"
	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/runtime"
	"github.com/solcreek/creekd/internal/sandbox"
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
