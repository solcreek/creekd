# observability

creekd exposes a Prometheus-format `/metrics` endpoint on the admin listener. This example boots two no-op apps and shows what the scrape returns, then walks through wiring it up to Prometheus / OpenTelemetry Collector / Grafana Cloud Agent.

## What it shows

- **Per-app cgroup state at scrape time**: `memory.current`, `memory.max`, `pids.current`, `cpu.usage`, `oom_kill` count. No background goroutines; the cgroup files are read on every scrape.
- **Push counters from the dispatch reverse proxy**: bytes sent + request count per app, labeled by HTTP status code.
- **Daemon-level rollup**: apps by status, build version.
- **Bearer-token guarded** on the admin listener (the same `CREEKD_ADMIN_TOKEN` that gates the rest of `/v1/*`).

## Run it

```bash
./up.sh
# ==> starts creekd, spawns app-a + app-b, writes a random token to .token

curl -H "Authorization: Bearer $(cat .token)" http://127.0.0.1:9080/metrics
```

## What you'll see

```text
# HELP creekd_build_info Build version stamped once at daemon startup. Always 1.
# TYPE creekd_build_info gauge
creekd_build_info{version="0.4.0"} 1

# HELP creekd_apps Number of supervised apps by status.
# TYPE creekd_apps gauge
creekd_apps{status="crash-looping"} 0
creekd_apps{status="crashed"} 0
creekd_apps{status="running"} 2
creekd_apps{status="starting"} 0
creekd_apps{status="stopped"} 0
creekd_apps{status="unhealthy"} 0

# HELP creekd_app_info Per-app static labels. Always 1.
# TYPE creekd_app_info gauge
creekd_app_info{app_id="app-a",runtime="",status="running"} 1
creekd_app_info{app_id="app-b",runtime="",status="running"} 1

# HELP creekd_app_memory_current_bytes Cgroup memory.current — current resident memory of the app's cgroup.
# TYPE creekd_app_memory_current_bytes gauge
creekd_app_memory_current_bytes{app_id="app-a"} 540672
creekd_app_memory_current_bytes{app_id="app-b"} 536576

# HELP creekd_app_restart_count Total restart count since the app was spawned. Reset on supervisor restart.
# TYPE creekd_app_restart_count gauge
creekd_app_restart_count{app_id="app-a"} 0
creekd_app_restart_count{app_id="app-b"} 0

# HELP creekd_dispatch_bytes_sent_total Bytes written to clients through the dispatch reverse proxy per app.
# TYPE creekd_dispatch_bytes_sent_total counter
# (only emitted after at least one dispatched request lands)
```

The dispatch counters appear once you generate traffic through `127.0.0.1:9000`:

```bash
curl -H 'X-Creek-App: app-a' http://127.0.0.1:9000/
curl -H "Authorization: Bearer $(cat .token)" http://127.0.0.1:9080/metrics | grep dispatch
# creekd_dispatch_bytes_sent_total{app_id="app-a"} 92
# creekd_dispatch_requests_total{app_id="app-a",code="503"} 1
```

(503 because the no-op `sleep` doesn't actually serve HTTP — fine for the demo, in reality the codes will reflect your apps' responses.)

## Prometheus scrape config

```yaml
scrape_configs:
  - job_name: creekd
    scrape_interval: 30s
    static_configs:
      - targets: ['localhost:9080']
    authorization:
      type: Bearer
      credentials_file: /etc/creekd/admin-token  # or `credentials:` inline
    metrics_path: /metrics
    scheme: http
```

Bearer token via `credentials_file` is recommended for production — keeps the secret out of the Prometheus config file itself.

## OpenTelemetry Collector — scrape Prom and forward as OTLP

If your aggregation stack is OTLP (Tempo / Honeycomb / Grafana Cloud / Datadog OTLP), the OTel Collector's Prometheus receiver scrapes us cleanly and re-exports as OTLP — no creekd config change needed.

```yaml
receivers:
  prometheus:
    config:
      scrape_configs:
        - job_name: creekd
          scrape_interval: 30s
          static_configs:
            - targets: ['localhost:9080']
          authorization:
            type: Bearer
            credentials: ${env:CREEKD_ADMIN_TOKEN}
          metrics_path: /metrics

exporters:
  otlp:
    endpoint: tempo.example.com:4317

service:
  pipelines:
    metrics:
      receivers: [prometheus]
      exporters: [otlp]
```

## Grafana Cloud Agent / Alloy

Identical Prom scrape config — Grafana Alloy speaks Prometheus natively. Drop the snippet under `prometheus.scrape "creekd" { ... }` block.

## Datadog Agent

The Datadog Agent's OpenMetrics check (`openmetrics`) consumes `/metrics` directly. In `conf.d/openmetrics.d/conf.yaml`:

```yaml
instances:
  - openmetrics_endpoint: http://localhost:9080/metrics
    namespace: creekd
    metrics:
      - "creekd_*"
    headers:
      Authorization: Bearer ${DD_ENV_CREEKD_ADMIN_TOKEN}
```

## What metrics are useful for

| Metric | Operator use |
|---|---|
| `creekd_app_memory_current_bytes` | Alert when an app approaches its `memory.max` cap; correlate restart events to memory growth |
| `creekd_app_oom_kills_total` | Page on any non-zero — means the hard cap fired, app got killed |
| `creekd_app_restart_count` | Track flapping apps. Sudden jump = crash loop |
| `creekd_app_health_failures_total` | Sustained increase = upstream backend is sick |
| `creekd_dispatch_bytes_sent_total` | Per-tenant bandwidth. Capacity planning, fairness, anti-abuse signals |
| `creekd_dispatch_requests_total{code="5xx"}` | Per-app error rate; alert on rate spikes |
| `creekd_apps{status="crash-looping"}` | Any non-zero is a problem |

## Grafana dashboard

A starter dashboard ships in [`grafana-dashboard.json`](grafana-dashboard.json). It covers:

- Apps by status (running / crashed / crash-looping / etc.)
- Lifetime OOM-kill count and creekd version stat
- Per-app memory current + cap-utilisation ratio (with 70%/90% threshold lines)
- Request rate + 5xx error rate per app
- Dispatch bytes/sec per app
- Per-app restart + health-failure table

To import: **Grafana → Dashboards → New → Import**, paste the JSON, pick your Prometheus datasource at the prompt. The dashboard uses a single `app` template variable so you can filter to one app or view the whole fleet.

Schema version targets Grafana 10+. If your Grafana is older, the panels still render but some `cellOptions` styling falls back to default.

## Alert rules

[`alerts.yml`](alerts.yml) ships a starter rule group. Six alerts, three severities:

| Alert | Expression | Severity |
|---|---|---|
| `AppOOMKilled` | `increase(creekd_app_oom_kills_total[5m]) > 0` | critical |
| `AppCrashLooping` | `creekd_apps{status="crash-looping"} > 0` | critical |
| `AppFlapping` | `rate(creekd_app_restart_count[10m]) > 0.005` | warning |
| `AppApproachingMemoryCap` | `memory_current / memory_max > 0.9` | warning |
| `Elevated5xx` | `5xx rate / total rate > 0.05` | warning |
| `HealthCheckFailing` | `rate(creekd_app_health_failures_total[5m]) > 0` | info |

To load: drop the file in your `rule_files:` directive (Prometheus) or paste into a Grafana managed rule group. Each rule has `annotations.runbook` with the operator-facing fix.

These are starters, not gospel — tune `for:` durations and thresholds for your traffic pattern.

## Why Prometheus format (not OTel SDK)

Three reasons, in priority order:

1. **Convergence**: Prometheus 3.0 (Nov 2024) added native OTLP ingestion at `/api/v1/otlp/v1/metrics`; the OTel Collector's `prometheusreceiver` has always scraped Prom. Picking either format reaches both audiences without a creekd-side decision.
2. **Industry convention for our slot**: Caddy, Traefik, HAProxy, etcd, Kubernetes — every infrastructure-tier tool with a default metrics endpoint emits Prom. The OTel SDK is application instrumentation, not infrastructure observability.
3. **Stability**: `prometheus/client_golang`'s API has been stable since 2017; the OTel Go SDK is officially Stable but its governance committee publicly acknowledges ongoing churn (epoch-release proposal still in flight).

## Limits — what this example *doesn't* show

- **No alerting rules**: this is the substrate. Alerting/SLO definitions belong in your Prom / Grafana stack, not in creekd.
- **No quota enforcement**: creekd intentionally measures, it doesn't enforce. Quota is a policy decision — billing tier, free-tier protection, anti-abuse — that should consume these metrics, not live inside creekd. The bytes counter is exposed; what to do when an app crosses 25 GB/month is your business layer's call.
- **No exemplars / trace linking**: tracing is a separate concern and not bundled here.
