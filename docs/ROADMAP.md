# Roadmap

> Public-facing summary of `creekd` development plan. Detailed strategic context lives in the private product-planning archive; this is the engineering view.

## Status

**Phase 1 — Daemon Hardening (M5)**, in progress.

## M5: From PoC multiplexer to production supervisor

The PoC version of creekd (in the main creek monorepo, TypeScript) runs all apps inside a single Bun process via dynamic `import()`. That's fine for prototype; it cannot reach production:

- One app crash kills all apps
- No process-level isolation (cgroup not possible)
- Cannot mix runtimes (Bun-only)
- No graceful zero-downtime deploys
- No per-app resource limits

M5 rewrites creekd in Go as a real process supervisor. Each app runs as an isolated child process. creekd watches lifecycles, routes HTTP traffic, enforces cgroup limits, captures logs, and does blue-green deploys.

### Sub-milestones

| Milestone | What | Effort |
|---|---|---|
| M5.1 | Child-process spawn + basic supervision | 5-7 days |
| M5.2 | Restart policy (exponential backoff + crash-loop detection) | 3-4 days |
| M5.3 | Health probe + graceful shutdown | 4-5 days |
| M5.4 | Multi-runtime dispatch (Bun/Node/Deno) | 7-10 days |
| M5.5 | Per-tenant cgroup v2 enforcement | 5-7 days |
| M5.6 | Log capture + rotation + structured JSON | 3-4 days |
| M5.7 | Zero-downtime blue-green deploy | 5-7 days |
| Soak test | 24-hour soak with 50 fake tenants | 5-7 days |

Total: **5-7 weeks solo**.

### Critical path

M5.1 → M5.2 → M5.3 → M5.7 (process model + lifecycle + deploy). M5.4/M5.5/M5.6 can be parallelized.

## Beyond M5 (Phase 1)

Once M5 lands, creekd hands off to M6 (self-host packaging), M7 (web dashboard polish), M8 (hosted alpha), M9 (public beta). These touch creekd but the heavy work is in the rest of the stack:

- **M6**: Docker image, systemd unit, install script
- **M7**: Web dashboard talks to creekd via gRPC
- **M8**: Hosted infrastructure (EU + US regions, multiple creekd hosts)
- **M9**: Public beta launch with full self-serve

## Phase 2 (post-launch)

- Nixpacks-based build pipeline
- BYO Dockerfile (general-PaaS expansion)
- `reusePort` cluster mode for Team+ tier
- Managed Postgres
- Asia-Pacific region
- OpenTelemetry export

## Phase 3+ (speculative)

- Multi-language hosting (Python, Go, Rust)
- Possible Rust rewrite of hot paths in creekd if benchmarks demand
- Geographic edge deployment

## How to contribute

This repo is private during Phase 1 (estimated until Nov 2026 launch). Contribution model will open at Phase 1 public beta.

Until then, follow `solcreek` on GitHub for the public launch.
