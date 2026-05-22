# M5 Soak Test

24-hour production-readiness test with 50 tenants. M5 acceptance gate.

## Acceptance criteria (from phase-1-roadmap.md)

> 24-hour soak test with 50 fake tenants (mix of Bun+Node), no neighbor
> disruption on crash, restart <500ms

## What it validates

| # | Requirement | How we measure |
|---|---|---|
| 1 | 50 tenants run simultaneously | `creekctl ps` shows 50 running |
| 2 | Mix of Bun + Node runtimes | 30 Bun + 20 Node (production-realistic ratio) |
| 3 | No neighbor disruption on crash | Crash one tenant every 5 min, verify others respond |
| 4 | Restart < 500ms | Measure time from crash to healthy response |
| 5 | No memory leak in creekd | RSS delta over 24h < 10MB |
| 6 | cgroup limits enforced | OOM tenant doesn't affect neighbors |
| 7 | Health probes recover | Flaky app recovers after health threshold |
| 8 | Blue-green deploy works mid-soak | Deploy 5 apps mid-test, verify zero-downtime |
| 9 | State survives daemon restart | Kill creekd, restart, all 50 apps come back |
| 10 | Logs don't fill disk | Log rotation keeps per-app logs bounded |

## Architecture

```
Linux host (Hetzner CX22: 2 vCPU, 4GB RAM, or Lima VM)
  └── creekd (cgroup parent: creekd-soak.slice)
        ├── tenant-001 (Bun, port 19001, 64MB cap)
        ├── tenant-002 (Node, port 19002, 64MB cap)
        ├── ...
        └── tenant-050 (Bun, port 19050, 64MB cap)

Orchestrator (Go binary):
  - Phase 1: spawn 50 tenants
  - Phase 2: run 24h with continuous probing
  - Phase 3: inject faults (crash, OOM, deploy)
  - Phase 4: verify all pass criteria
  - Emit NDJSON metrics every 60s
```

## Usage

```bash
# Build everything
./bootstrap.sh

# Run soak test (default: 24h, 50 tenants)
./run.sh

# Quick smoke (10 min, 10 tenants — for CI or dev iteration)
DURATION=10m TENANTS=10 ./run.sh

# Monitor live
tail -f soak-metrics.ndjson | jq .

# Results
cat RESULTS.md
```

## Tenant app

Each tenant runs a lightweight HTTP server that:
- Serves `GET /` with `{"id":"tenant-NNN","runtime":"bun","pid":1234}`
- Serves `GET /health` (200 OK)
- Serves `GET /crash` (exits with code 1 — tests auto-restart)
- Serves `GET /leak?mb=N` (allocates N MB — tests cgroup OOM)
- Uses ~8-15MB baseline RAM (Bun) or ~30-50MB (Node)
