# Roadmap

> Public-facing summary of `creekd` development plan. Detailed strategic context lives in the private product-planning archive; this is the engineering view.

## Status

**Phase 1 — `v0.1.x` shipped.** Latest tag: `v0.1.1` (2026-05-19). The
daemon-hardening milestone (M5) and the supply-chain / governance
follow-ups merged in subsequent stack PRs are on `main`; the next
tagged release will bundle the `[Unreleased]` block in
[`CHANGELOG.md`](../CHANGELOG.md) (envelope/CAS API, audit-WAL hash
chain, release ledger + state.json v3, TOFU hostkey, systemd
hardening, cosign + SLSA L3 provenance, `creekctl self-upgrade`,
adminapi handler hardening).

## M5: From PoC multiplexer to production supervisor — shipped

The PoC version of creekd (in the main creek monorepo, TypeScript) ran all apps inside a single Bun process via dynamic `import()`. That's fine for prototype; it cannot reach production:

- One app crash kills all apps
- No process-level isolation (cgroup not possible)
- Cannot mix runtimes (Bun-only)
- No graceful zero-downtime deploys
- No per-app resource limits

M5 rewrote creekd in Go as a real process supervisor. Each app runs as an isolated child process. creekd watches lifecycles, routes HTTP traffic, enforces cgroup limits, captures logs, and does blue-green deploys.

### Sub-milestones (all shipped)

| Milestone | What | Status |
|---|---|---|
| M5.1 | Child-process spawn + basic supervision | shipped (v0.1.0) |
| M5.2 | Restart policy (exponential backoff + crash-loop detection) | shipped (v0.1.0) |
| M5.3 | Health probe + graceful shutdown | shipped (v0.1.0) |
| M5.4 | Multi-runtime dispatch (Bun/Node/Deno) | shipped (v0.1.0) |
| M5.5 | Per-tenant cgroup v2 enforcement | shipped (v0.1.0) |
| M5.6 | Log capture + rotation + structured JSON | shipped (v0.1.0) |
| M5.7 | Zero-downtime blue-green deploy | shipped (v0.1.0) |
| Soak test | 24-hour soak with 50 fake tenants | shipped (v0.1.0) |

## Post-M5 hardening (on `main`, awaiting tag)

Work landed on `main` since `v0.1.1`. Each item links the PR that landed it:

- **OpenAPI 3.0 spec → Go types/server** ([#4](https://github.com/solcreek/creekd/pull/4)) — `api/openapi.yaml` is now the source-of-truth wire format; Go types + server interface are generated.
- **K8s-style envelope + If-Match CAS** ([#5](https://github.com/solcreek/creekd/pull/5)) — `apiVersion / kind / metadata / spec / status` shape, `status.conditions[]` (Ready / Progressing / Degraded / BackupReady), optimistic-concurrency middleware.
- **Durability hardening** ([#6](https://github.com/solcreek/creekd/pull/6)) — fsync, per-app locks, audit-log WAL with pending/commit semantics.
- **Audit log hash chain + Tier 0 backup** ([#7](https://github.com/solcreek/creekd/pull/7)) — append-only audit log with previous-record SHA-256 chaining; backup tarball + signed `MANIFEST.json` + ed25519 hostkey.
- **Release ledger + state.json v3** ([#8](https://github.com/solcreek/creekd/pull/8)) — every deploy / rollback recorded with config snapshot + env hash; rollback re-runs an exact prior configuration.
- **TOFU hostkey + systemd hardening** ([#9](https://github.com/solcreek/creekd/pull/9)) — unauthenticated `GET /v1/hostkey` for first-time-use pinning; canonical systemd directives + `creekctl hardening-check` drift validator.
- **Cosign keyless + SLSA L3 provenance** ([#10](https://github.com/solcreek/creekd/pull/10)) — releases signed via Fulcio + Rekor; SLSA generator workflow attests every artifact.
- **`creekctl self-upgrade` + governance docs** ([#11](https://github.com/solcreek/creekd/pull/11)) — verify (cosign + SHA-256) then atomically swap binaries; `NON-GOALS.md`, `MAINTAINERS.md`, `CONTRIBUTING.md`, `INSTALL.md`, ADR scaffold.
- **adminapi handler hardening** ([#16](https://github.com/solcreek/creekd/pull/16)) — port-range validation at the handler boundary, 413 + `request_too_large` error code, ephemeral envelope metadata when no store is configured, SSE blank-line terminator.

## Sandbox mode (Phase 1)

`creekd sandbox ./app` runs the same daemon locally for development.
Not a simulator — same process supervisor, same bindings API, same
health probes. Production-irrelevant features (cgroups, TLS, DNS)
are skipped; file-watch auto-restart is added.

Primitives auto-provision as local equivalents:
- `database` → SQLite (`~/.creekd/sandbox/<app>/db.sqlite`)
- `cache` → SQLite KV
- `storage` → local filesystem

Designed for AI agent workflows: one command, zero config files,
no interactive prompts, structured JSON output via `--json` flag.

```
Agent loop:
  creekd sandbox ./app       ← full environment, one command
  edit → auto-restart → verify on localhost
  creekctl deploy --from manifest.json   ← ship to production
```

## Event stream — shipped

`GET /v1/apps/{id}/events` is live as an SSE endpoint streaming app state transitions. Eliminates polling for agent monitoring workflows. Spec-compliant `\n\n` event terminator (fixed in [#16](https://github.com/solcreek/creekd/pull/16)); client-disconnect detection on the write path so disconnects free the subscription instead of pinning the handler.

```
data: {"type":"status_changed","status":"running","pid":1234,"ts":"..."}
data: {"type":"health_failure","consecutive":3,"ts":"..."}
data: {"type":"oom_kill","memory_max_bytes":268435456,"ts":"..."}
data: {"type":"restart","restart_count":5,"ts":"..."}
```

Agent workflow:
```
creekctl ensure my-app ...  → spawn/no-op
creekctl events my-app      → stream status changes (blocks until event)
```

## Beyond `v0.1.x`

Next work areas (not yet milestoned):

- **External dashboard** that drives the JSON admin API. Note: an
  in-daemon web UI is a documented non-goal — see
  [`NON-GOALS.md`](../NON-GOALS.md) §N2. A separate process (curl /
  TUI / standalone dashboard) is the supported shape.
- **Hosted infrastructure** (EU + US regions, multiple creekd hosts) when the self-host story is stable.
- **Public beta launch** with full self-serve.

## Phase 2 (post-launch)

- Unified build pipeline: Dockerfile-first + Nixpacks fallback (both produce OCI image; creekd runs one artifact type regardless of build path)
- Sandbox: production-faithful primitive provisioning (real Postgres, Redis)
- `reusePort` cluster mode for Team+ tier
- Managed Postgres
- Asia-Pacific region
- OpenTelemetry export

## Phase 3+ (speculative)

- Multi-language hosting (Python, Go, Rust)
- Possible Rust rewrite of hot paths in creekd if benchmarks demand
- Geographic edge deployment

## How to contribute

The repo is **public** under Apache 2.0; see [`CONTRIBUTING.md`](../CONTRIBUTING.md) for the DCO sign-off, ADR process, and review expectations, and [`MAINTAINERS.md`](../MAINTAINERS.md) for the bus-factor + escalation story.
