// Package metrics exposes creekd's runtime state via a Prometheus
// /metrics endpoint. The package is observability-only: it measures,
// it does not enforce. Quota decisions, billing, throttling — those
// are policy layers that should consume these metrics, not live
// inside creekd.
//
// Two shapes of metric live here:
//
//   - Lazy collectors (cgroup memory.current, pids, cpu, oom_kills,
//     restart counts, etc.) — read on every Prometheus scrape via
//     prometheus.Collector. No background goroutine, no maintained
//     state; the cgroup files are the source of truth.
//
//   - Push counters (dispatch bytes-sent, request totals) — fed from
//     dispatch.ResponseObserver as each request completes. CounterVec
//     holds the state.
//
// Wire-up in main:
//
//	m := metrics.New(sup, version)
//	router.SetObserver(m.ObserveDispatch)
//	mux.Handle("GET /metrics", m.Handler())
//
// Endpoint exposition format is Prometheus text + OpenMetrics
// (negotiated via Accept header). Compatible with `prometheus`,
// the OpenTelemetry Collector's prometheusreceiver, Grafana Cloud
// Agent, Datadog Agent, Vector — anything that scrapes Prom.
package metrics

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/solcreek/creekd/internal/supervisor"
)

// Metrics owns creekd's Prometheus registry and the push-counter
// vectors fed from dispatch.
type Metrics struct {
	Registry *prometheus.Registry

	dispatchBytes    *prometheus.CounterVec
	dispatchRequests *prometheus.CounterVec
}

// New builds a fresh registry, registers the supervisor collector,
// and stamps creekd_build_info with the given version string.
func New(sup *supervisor.Supervisor, version string) *Metrics {
	reg := prometheus.NewRegistry()

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "creekd_build_info",
		Help: "Build version stamped once at daemon startup. Always 1.",
	}, []string{"version"})
	buildInfo.WithLabelValues(version).Set(1)
	reg.MustRegister(buildInfo)

	dispatchBytes := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "creekd_dispatch_bytes_sent_total",
		Help: "Bytes written to clients through the dispatch reverse proxy per app.",
	}, []string{"app_id"})
	reg.MustRegister(dispatchBytes)

	dispatchRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "creekd_dispatch_requests_total",
		Help: "Requests dispatched per app and HTTP status code.",
	}, []string{"app_id", "code"})
	reg.MustRegister(dispatchRequests)

	reg.MustRegister(newSupervisorCollector(sup))

	return &Metrics{
		Registry:         reg,
		dispatchBytes:    dispatchBytes,
		dispatchRequests: dispatchRequests,
	}
}

// Handler returns the /metrics http.Handler. Content negotiation:
// text/plain (Prometheus) by default, application/openmetrics-text
// when the client requests it via Accept.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// ObserveDispatch is the dispatch.ResponseObserver implementation.
// Called once per dispatched request after the proxy returns.
func (m *Metrics) ObserveDispatch(appID string, bytesOut int64, statusCode int) {
	if bytesOut > 0 {
		m.dispatchBytes.WithLabelValues(appID).Add(float64(bytesOut))
	}
	m.dispatchRequests.WithLabelValues(appID, strconv.Itoa(statusCode)).Inc()
}

// supervisorCollector implements prometheus.Collector for per-app
// supervisor + cgroup stats. Read lazily at scrape time — the cgroup
// files are the source of truth, no caching.
type supervisorCollector struct {
	sup *supervisor.Supervisor

	memCurrent     *prometheus.Desc
	memMax         *prometheus.Desc
	pidsCurrent    *prometheus.Desc
	cpuUsage       *prometheus.Desc
	oomKills       *prometheus.Desc
	restartCount   *prometheus.Desc
	healthFailures *prometheus.Desc
	info           *prometheus.Desc
	apps           *prometheus.Desc
}

func newSupervisorCollector(sup *supervisor.Supervisor) *supervisorCollector {
	return &supervisorCollector{
		sup: sup,
		memCurrent: prometheus.NewDesc("creekd_app_memory_current_bytes",
			"Cgroup memory.current — current resident memory of the app's cgroup.",
			[]string{"app_id"}, nil),
		memMax: prometheus.NewDesc("creekd_app_memory_max_bytes",
			"Cgroup memory.max — hard cap configured for the app (0 if unset).",
			[]string{"app_id"}, nil),
		pidsCurrent: prometheus.NewDesc("creekd_app_pids_current",
			"Cgroup pids.current — number of processes inside the app's cgroup.",
			[]string{"app_id"}, nil),
		cpuUsage: prometheus.NewDesc("creekd_app_cpu_usage_seconds_total",
			"Cgroup cpu.usage_usec converted to seconds, monotonic since cgroup creation.",
			[]string{"app_id"}, nil),
		oomKills: prometheus.NewDesc("creekd_app_oom_kills_total",
			"Cgroup memory.events oom_kill count — processes killed by cgroup-scope OOM.",
			[]string{"app_id"}, nil),
		restartCount: prometheus.NewDesc("creekd_app_restart_count",
			"Total restart count since the app was spawned. Reset on supervisor restart.",
			[]string{"app_id"}, nil),
		healthFailures: prometheus.NewDesc("creekd_app_health_failures_total",
			"Cumulative failed health probes since the app was spawned.",
			[]string{"app_id"}, nil),
		info: prometheus.NewDesc("creekd_app_info",
			"Per-app static labels. Always 1.",
			[]string{"app_id", "runtime", "status"}, nil),
		apps: prometheus.NewDesc("creekd_apps",
			"Number of supervised apps by status.",
			[]string{"status"}, nil),
	}
}

func (c *supervisorCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.memCurrent
	ch <- c.memMax
	ch <- c.pidsCurrent
	ch <- c.cpuUsage
	ch <- c.oomKills
	ch <- c.restartCount
	ch <- c.healthFailures
	ch <- c.info
	ch <- c.apps
}

func (c *supervisorCollector) Collect(ch chan<- prometheus.Metric) {
	apps := c.sup.List()

	// Daemon-level: count by status. Always emit known statuses (even
	// at 0) so dashboards don't have to guess label sets.
	counts := map[string]int{
		"starting":      0,
		"running":       0,
		"crashed":       0,
		"crash-looping": 0,
		"stopped":       0,
		"unhealthy":     0,
	}
	for _, app := range apps {
		counts[app.Status().String()]++
	}
	for status, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.apps, prometheus.GaugeValue,
			float64(n), status)
	}

	// Per-app.
	for _, app := range apps {
		ch <- prometheus.MustNewConstMetric(c.info, prometheus.GaugeValue, 1,
			app.ID, string(app.Runtime), app.Status().String())
		ch <- prometheus.MustNewConstMetric(c.restartCount, prometheus.GaugeValue,
			float64(app.RestartCount()), app.ID)
		ch <- prometheus.MustNewConstMetric(c.healthFailures, prometheus.CounterValue,
			float64(app.HealthFailures()), app.ID)

		cg := app.Cgroup()
		if cg == nil {
			continue
		}
		// Cgroup reads can fail individually (file briefly missing during
		// restart, permission churn). Skip those silently — the next
		// scrape will pick up the recovered value. We don't want a
		// transient read error to drop the whole app's row.
		if cur, err := cg.MemoryCurrent(); err == nil {
			ch <- prometheus.MustNewConstMetric(c.memCurrent, prometheus.GaugeValue,
				float64(cur), app.ID)
		}
		if max, err := cg.MemoryMax(); err == nil {
			ch <- prometheus.MustNewConstMetric(c.memMax, prometheus.GaugeValue,
				float64(max), app.ID)
		}
		if pids, err := cg.PidsCurrent(); err == nil {
			ch <- prometheus.MustNewConstMetric(c.pidsCurrent, prometheus.GaugeValue,
				float64(pids), app.ID)
		}
		if usec, err := cg.CPUUsageMicros(); err == nil {
			ch <- prometheus.MustNewConstMetric(c.cpuUsage, prometheus.CounterValue,
				float64(usec)/1e6, app.ID)
		}
		if st, err := cg.Stats(); err == nil {
			ch <- prometheus.MustNewConstMetric(c.oomKills, prometheus.CounterValue,
				float64(st.OOMKill), app.ID)
		}
	}
}
