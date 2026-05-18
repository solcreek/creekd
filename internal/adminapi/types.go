package adminapi

import (
	"time"

	"github.com/solcreek/creekd/internal/cgroup"
	"github.com/solcreek/creekd/internal/runtime"
	"github.com/solcreek/creekd/internal/supervisor"
)

// SpawnRequest is the body of POST /v1/apps. ID, Port are required.
// Exactly one of (Command + Args) or (Runtime + Entry) must be set;
// see supervisor.Config for the resolution rules.
type SpawnRequest struct {
	ID      string   `json:"id"`
	Runtime string   `json:"runtime,omitempty"`
	Entry   string   `json:"entry,omitempty"`
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	Port    int      `json:"port"`
	Env     []string `json:"env,omitempty"`
	Limits  *Limits  `json:"limits,omitempty"`
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
	Runtime         string   `json:"runtime,omitempty"`
	Entry           string   `json:"entry,omitempty"`
	Command         string   `json:"command,omitempty"`
	Args            []string `json:"args,omitempty"`
	Port            int      `json:"port"`
	Env             []string `json:"env,omitempty"`
	Limits          *Limits  `json:"limits,omitempty"`
	ReadyTimeoutMS  int64    `json:"ready_timeout_ms,omitempty"`
	PollIntervalMS  int64    `json:"poll_interval_ms,omitempty"`
	GracefulV1MS    int64    `json:"graceful_v1_ms,omitempty"`
}

// Limits mirrors cgroup.Limits in the JSON wire format. Zero fields
// mean "no limit" — same semantic as the cgroup package.
type Limits struct {
	MemoryMaxBytes int64 `json:"memory_max_bytes,omitempty"`
	PidsMax        int64 `json:"pids_max,omitempty"`
	CPUQuotaUS     int64 `json:"cpu_quota_us,omitempty"`
	CPUPeriodUS    int64 `json:"cpu_period_us,omitempty"`
}

// toCgroupLimits maps the API Limits to the cgroup-internal type, or
// returns nil if the API value is nil or zero.
func (l *Limits) toCgroupLimits() *cgroup.Limits {
	if l == nil {
		return nil
	}
	if l.MemoryMaxBytes == 0 && l.PidsMax == 0 && l.CPUQuotaUS == 0 {
		return nil
	}
	return &cgroup.Limits{
		MemoryMax: l.MemoryMaxBytes,
		PidsMax:   l.PidsMax,
		CPUQuota:  l.CPUQuotaUS,
		CPUPeriod: l.CPUPeriodUS,
	}
}

// AppView is the JSON representation of a supervised app — returned
// by GET endpoints and by successful spawn/deploy responses.
type AppView struct {
	ID             string `json:"id"`
	Runtime        string `json:"runtime,omitempty"`
	Command        string `json:"command"`
	Args           []string `json:"args,omitempty"`
	Port           int    `json:"port"`
	Status         string `json:"status"`
	PID            int    `json:"pid"`
	UptimeMS       int64  `json:"uptime_ms"`
	RestartCount   int    `json:"restart_count"`
	HealthFailures int64  `json:"health_failures"`
}

// viewOf snapshots an *supervisor.App into an AppView. Returns the
// zero value if app is nil.
func viewOf(app *supervisor.App) AppView {
	if app == nil {
		return AppView{}
	}
	return AppView{
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
}

// ListResponse is the body of GET /v1/apps.
type ListResponse struct {
	Apps []AppView `json:"apps"`
}

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

// Error codes used in ErrorResponse.Code.
const (
	CodeBadRequest   = "bad_request"
	CodeUnauthorized = "unauthorized"
	CodeNotFound     = "not_found"
	CodeConflict     = "conflict"
	CodeUnhealthy    = "deploy_unhealthy"
	CodeInternal     = "internal"
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
