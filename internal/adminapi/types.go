package adminapi

import (
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/runtime"
	"github.com/solcreek/creekd/internal/sandbox"
	"github.com/solcreek/creekd/internal/supervisor"
)

// SpawnRequest is the body of POST /v1/apps. ID, Port are required.
// Exactly one of (Command + Args) or (Runtime + Entry) must be set;
// see supervisor.Config for the resolution rules.
type SpawnRequest struct {
	ID              string        `json:"id"`
	Runtime         string        `json:"runtime,omitempty"`
	Entry           string        `json:"entry,omitempty"`
	Command         string        `json:"command,omitempty"`
	Args            []string      `json:"args,omitempty"`
	Port            int           `json:"port"`
	Env             []string      `json:"env,omitempty"`
	Limits          *Limits       `json:"limits,omitempty"`
	NetIsolation    bool          `json:"net_isolation,omitempty"`
	Sandbox         *Sandbox      `json:"sandbox,omitempty"`
	HealthCheckPath string        `json:"health_check_path,omitempty"`
	VolumeMounts    []VolumeMount `json:"volume_mounts,omitempty"`
}

// RestartRequest is the body of POST /v1/apps/{id}/restart. An empty
// body is accepted — TimeoutMS defaults to 10s (handled in
// supervisor.Restart).
type RestartRequest struct {
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// DeployRequest is the body of POST /v1/apps/{id}/deploy. The path
// identifies the v1 app; the body describes v2. Port must differ
// from v1's current port.
type DeployRequest struct {
	Runtime         string        `json:"runtime,omitempty"`
	Entry           string        `json:"entry,omitempty"`
	Command         string        `json:"command,omitempty"`
	Args            []string      `json:"args,omitempty"`
	Port            int           `json:"port"`
	Env             []string      `json:"env,omitempty"`
	Limits          *Limits       `json:"limits,omitempty"`
	NetIsolation    bool          `json:"net_isolation,omitempty"`
	Sandbox         *Sandbox      `json:"sandbox,omitempty"`
	HealthCheckPath string        `json:"health_check_path,omitempty"`
	VolumeMounts    []VolumeMount `json:"volume_mounts,omitempty"`
	ReadyTimeoutMS  int64         `json:"ready_timeout_ms,omitempty"`
	PollIntervalMS  int64         `json:"poll_interval_ms,omitempty"`
	GracefulV1MS    int64         `json:"graceful_v1_ms,omitempty"`
}

// Limits mirrors cgroup.Limits in the JSON wire format. Zero fields
// mean "no limit" — same semantic as the cgroup package.
//
// MemoryHigh is the soft cap (throttle without OOM-kill) — recommended
// for noisy-neighbor protection. MemoryMax is the hard cap (OOM-kill
// on overrun); pair with MemoryHigh as defense in depth.
type Limits struct {
	MemoryHighBytes int64 `json:"memory_high_bytes,omitempty"`
	MemoryMaxBytes  int64 `json:"memory_max_bytes,omitempty"`
	PidsMax         int64 `json:"pids_max,omitempty"`
	CPUQuotaUS      int64 `json:"cpu_quota_us,omitempty"`
	CPUPeriodUS     int64 `json:"cpu_period_us,omitempty"`
}

// toCgroupLimits maps the API Limits to the cgroup-internal type, or
// returns nil if the API value is nil or zero.
func (l *Limits) toCgroupLimits() *cgroup.Limits {
	if l == nil {
		return nil
	}
	if l.MemoryHighBytes == 0 && l.MemoryMaxBytes == 0 && l.PidsMax == 0 && l.CPUQuotaUS == 0 {
		return nil
	}
	return &cgroup.Limits{
		MemoryHigh: l.MemoryHighBytes,
		MemoryMax:  l.MemoryMaxBytes,
		PidsMax:    l.PidsMax,
		CPUQuota:   l.CPUQuotaUS,
		CPUPeriod:  l.CPUPeriodUS,
	}
}

// Sandbox is the wire-format counterpart of sandbox.Spec. Every
// field is opt-in (zero value = host-shared, no isolation). UID/GID
// mapping is not exposed here — callers needing it should construct
// a request body manually. v0.1.0 CLI surface keeps to the booleans
// + chroot.
type Sandbox struct {
	PIDNamespace   bool   `json:"pid_namespace,omitempty"`
	UTSNamespace   bool   `json:"uts_namespace,omitempty"`
	IPCNamespace   bool   `json:"ipc_namespace,omitempty"`
	MountNamespace bool   `json:"mount_namespace,omitempty"`
	UserNamespace  bool   `json:"user_namespace,omitempty"`
	NoNewPrivs     bool   `json:"no_new_privs,omitempty"`
	Chroot         string `json:"chroot,omitempty"`
}

// toSandboxSpec returns the supervisor-facing spec, or nil when the
// API value is nil or has no knob set.
func (s *Sandbox) toSandboxSpec() *sandbox.Spec {
	if s == nil {
		return nil
	}
	spec := sandbox.Spec{
		PIDNamespace:   s.PIDNamespace,
		UTSNamespace:   s.UTSNamespace,
		IPCNamespace:   s.IPCNamespace,
		MountNamespace: s.MountNamespace,
		UserNamespace:  s.UserNamespace,
		NoNewPrivs:     s.NoNewPrivs,
		Chroot:         s.Chroot,
	}
	if !spec.Any() {
		return nil
	}
	return &spec
}

// VolumeMount mirrors supervisor.VolumeMount in JSON. VolumeID
// references a Volume previously created via POST /v1/volumes;
// SubPath optionally narrows the bind to a subdirectory; Target
// is the path the child sees; ReadOnly tightens (never relaxes)
// the Volume's RO setting.
type VolumeMount struct {
	VolumeID string `json:"volume_id"`
	SubPath  string `json:"sub_path,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// toSupervisorVolumeMounts maps the API representation to the
// supervisor-internal type. Returns nil for nil/empty input.
func toSupervisorVolumeMounts(vms []VolumeMount) []supervisor.VolumeMount {
	if len(vms) == 0 {
		return nil
	}
	out := make([]supervisor.VolumeMount, len(vms))
	for i, v := range vms {
		out[i] = supervisor.VolumeMount{
			VolumeID: v.VolumeID,
			SubPath:  v.SubPath,
			Target:   v.Target,
			ReadOnly: v.ReadOnly,
		}
	}
	return out
}

// VolumeRequest is the body of POST /v1/volumes. BackingPath is
// relative to the supervisor's VolumeRoot (e.g. "tenant-a/pg-data").
// The directory must already exist on the host; creekd does not
// create tenant data dirs.
type VolumeRequest struct {
	ID          string `json:"id"`
	BackingPath string `json:"backing_path"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// VolumeView is the JSON shape of GET /v1/volumes responses.
type VolumeView struct {
	ID          string `json:"id"`
	BackingPath string `json:"backing_path"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	FSType      string `json:"fs_type,omitempty"`
}

// VolumesListResponse is the body of GET /v1/volumes.
type VolumesListResponse struct {
	Volumes []VolumeView `json:"volumes"`
}

// volumeViewOf snapshots a supervisor.Volume into a VolumeView.
func volumeViewOf(v supervisor.Volume) VolumeView {
	return VolumeView{
		ID:          v.ID,
		BackingPath: v.BackingPath,
		ReadOnly:    v.ReadOnly,
		FSType:      v.FSType,
	}
}

// AppView is the JSON representation of a supervised app — returned
// by GET endpoints and by successful spawn/deploy responses.
type AppView struct {
	ID             string   `json:"id"`
	Runtime        string   `json:"runtime,omitempty"`
	Command        string   `json:"command"`
	Args           []string `json:"args,omitempty"`
	Port           int      `json:"port"`
	Status         string   `json:"status"`
	PID            int      `json:"pid"`
	UptimeMS       int64    `json:"uptime_ms"`
	RestartCount   int      `json:"restart_count"`
	HealthFailures int64    `json:"health_failures"`
	// NetIP is the container-side IP when the app was spawned with
	// NetIsolation. Empty for host-network apps.
	NetIP string `json:"net_ip,omitempty"`
}

// viewOf snapshots an *supervisor.App into an AppView. Returns the
// zero value if app is nil.
func viewOf(app *supervisor.App) AppView {
	if app == nil {
		return AppView{}
	}
	v := AppView{
		ID:             app.ID,
		Runtime:        string(app.Runtime),
		Command:        app.Command,
		Args:           append([]string(nil), app.Args...),
		Port:           app.Port,
		Status:         app.Status().String(),
		PID:            app.PID(),
		UptimeMS:       app.Uptime().Milliseconds(),
		RestartCount:   app.RestartCount(),
		HealthFailures: app.HealthFailures(),
	}
	if app.NetIP != nil {
		v.NetIP = app.NetIP.String()
	}
	return v
}

// ListResponse is the body of GET /v1/apps.
type ListResponse struct {
	Apps []AppView `json:"apps"`
}

// StatsView is the JSON shape of GET /v1/apps/{id}/stats. Counters
// reflect kernel-tracked accounting via cgroup v2 when
// CgroupEnabled is true; otherwise the supervisor only has
// OS-level state (uptime, restart count, health failures) which is
// already in AppView.
type StatsView struct {
	ID                 string `json:"id"`
	CgroupEnabled      bool   `json:"cgroup_enabled"`
	MemoryCurrentBytes int64  `json:"memory_current_bytes,omitempty"`
	MemoryMaxBytes     int64  `json:"memory_max_bytes,omitempty"`
	PidsCurrent        int64  `json:"pids_current,omitempty"`
	CPUUsageUsec       int64  `json:"cpu_usage_usec,omitempty"`
	OOMKills           int64  `json:"oom_kills,omitempty"`
	// ReadErr surfaces a stale-read failure (file briefly missing
	// during restart, permission churn). Non-fatal: the rest of the
	// snapshot is still trustworthy.
	ReadErr string `json:"read_err,omitempty"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

// Error codes used in ErrorResponse.Code. Agents branch on these
// values programmatically — keep them stable across versions.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized"
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"
	CodeAlreadyRunning  = "already_running"
	CodePortConflict    = "port_conflict"
	CodeInvalidID       = "invalid_id"
	CodeInvalidRuntime  = "invalid_runtime"
	CodeUnhealthy       = "deploy_unhealthy"
	CodeInternal        = "internal"
)

// parseRuntime returns the resolved Runtime or empty string if the
// input is empty (caller treats empty as "use explicit Command").
func parseRuntime(s string) (runtime.Runtime, error) {
	if s == "" {
		return "", nil
	}
	return runtime.Parse(s)
}

// msOr returns the duration represented by ms (in milliseconds) when
// ms > 0, otherwise fallback.
func msOr(ms int64, fallback time.Duration) time.Duration {
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}
