package adminapi

// Legacy types retained for adminclient and creekctl compatibility.
// The server handler now uses apitypes (generated from OpenAPI spec).
// JSON wire format is identical, so old clients can talk to the new
// server without changes.
//
// TODO: migrate adminclient + creekctl to use apitypes directly,
// then remove this file.

// SpawnRequest is the body of POST /v1/apps.
//
// Deprecated: use apitypes.SpawnRequest.
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

// RestartRequest is the body of POST /v1/apps/{id}/restart.
//
// Deprecated: use apitypes.RestartRequest.
type RestartRequest struct {
	TimeoutMS int64 `json:"timeout_ms,omitempty"`
}

// DeployRequest is the body of POST /v1/apps/{id}/deploy.
//
// Deprecated: use apitypes.DeployRequest.
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

// Limits mirrors cgroup.Limits in JSON wire format.
//
// Deprecated: use apitypes.Limits.
type Limits struct {
	MemoryHighBytes int64 `json:"memory_high_bytes,omitempty"`
	MemoryMaxBytes  int64 `json:"memory_max_bytes,omitempty"`
	PidsMax         int64 `json:"pids_max,omitempty"`
	CPUQuotaUS      int64 `json:"cpu_quota_us,omitempty"`
	CPUPeriodUS     int64 `json:"cpu_period_us,omitempty"`
}

// Sandbox mirrors sandbox.Spec in JSON wire format.
//
// Deprecated: use apitypes.Sandbox.
type Sandbox struct {
	PIDNamespace   bool   `json:"pid_namespace,omitempty"`
	UTSNamespace   bool   `json:"uts_namespace,omitempty"`
	IPCNamespace   bool   `json:"ipc_namespace,omitempty"`
	MountNamespace bool   `json:"mount_namespace,omitempty"`
	UserNamespace  bool   `json:"user_namespace,omitempty"`
	NoNewPrivs     bool   `json:"no_new_privs,omitempty"`
	Chroot         string `json:"chroot,omitempty"`
}

// VolumeMount references a registered volume for bind-mounting.
//
// Deprecated: use apitypes.VolumeMount.
type VolumeMount struct {
	VolumeID string `json:"volume_id"`
	SubPath  string `json:"sub_path,omitempty"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// VolumeRequest is the body of POST /v1/volumes.
//
// Deprecated: use apitypes.VolumeRequest.
type VolumeRequest struct {
	ID          string `json:"id"`
	BackingPath string `json:"backing_path"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// VolumeView is the JSON shape of GET /v1/volumes responses.
//
// Deprecated: use apitypes.VolumeView.
type VolumeView struct {
	ID          string `json:"id"`
	BackingPath string `json:"backing_path"`
	ReadOnly    bool   `json:"read_only,omitempty"`
	FSType      string `json:"fs_type,omitempty"`
}

// VolumesListResponse is the body of GET /v1/volumes.
//
// Deprecated: use apitypes.ListVolumesResponse.
type VolumesListResponse struct {
	Volumes []VolumeView `json:"volumes"`
}

// AppView is the JSON representation of a supervised app.
//
// Deprecated: use apitypes.AppView.
type AppView struct {
	ID             string   `json:"id"`
	Runtime        string   `json:"runtime,omitempty"`
	Command        string   `json:"command"`
	Args           []string `json:"args,omitempty"`
	Env            []string `json:"env,omitempty"`
	Port           int      `json:"port"`
	Status         string   `json:"status"`
	PID            int      `json:"pid"`
	UptimeMS       int64    `json:"uptime_ms"`
	RestartCount   int      `json:"restart_count"`
	HealthFailures int64    `json:"health_failures"`
	NetIP          string   `json:"net_ip,omitempty"`
}

// ListResponse is the body of GET /v1/apps.
//
// Deprecated: use apitypes.ListAppsResponse.
type ListResponse struct {
	Apps []AppView `json:"apps"`
}

// StatsView is the JSON shape of GET /v1/apps/{id}/stats.
//
// Deprecated: use apitypes.StatsView.
type StatsView struct {
	ID                 string `json:"id"`
	CgroupEnabled      bool   `json:"cgroup_enabled"`
	MemoryCurrentBytes int64  `json:"memory_current_bytes,omitempty"`
	MemoryMaxBytes     int64  `json:"memory_max_bytes,omitempty"`
	PidsCurrent        int64  `json:"pids_current,omitempty"`
	CPUUsageUsec       int64  `json:"cpu_usage_usec,omitempty"`
	OOMKills           int64  `json:"oom_kills,omitempty"`
	ReadErr            string `json:"read_err,omitempty"`
}

// ErrorResponse is the body of every non-2xx response.
//
// Deprecated: use apitypes.ErrorResponse.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

// Error codes.
const (
	CodeBadRequest     = "bad_request"
	CodeUnauthorized   = "unauthorized"
	CodeNotFound       = "not_found"
	CodeConflict       = "conflict"
	CodeAlreadyRunning = "already_running"
	CodePortConflict   = "port_conflict"
	CodeInvalidID      = "invalid_id"
	CodeInvalidRuntime = "invalid_runtime"
	CodeUnhealthy      = "deploy_unhealthy"
	CodeInternal       = "internal"
)
