# RFC: Stateful-Workload Substrate Extensions

Status: design 2026-05-20  (target: land before creekd v1.0 API freeze, ahead of Phase 1 Nov 2026 launch)

## Why this RFC exists

`creekd` v0.4.x supervises stateless processes well. Stateful workloads — Postgres, SQLite, MinIO, Valkey, NATS, anything that needs disk + non-HTTP networking + ordered start-up — need four substrate primitives that don't exist yet:

1. **Persistent volume mounts** (bind-mount declaration at spawn)
2. **Non-HTTP TCP dispatch** (port allocation + routing for non-HTTP protocols)
3. **Dependency-aware spawn ordering** (waiting for a dependency app to be ready before spawning a dependent)
4. **Snapshot / quiesce hook** (optional v1; for backup consistency in v2)

All four are **mechanism, not policy** — they extend what creekd can be told to do, without baking opinions about how to do anything in particular. This is the bar from `ARCHITECTURE.md` principle 2: substrate primitives are fair game; policies belong in callers (CLIs, orchestrators, dashboards).

These extensions are prerequisites for `STRATEGY-primitives.md`'s Layer 4 orchestrators (`creekdb`, `creek-sqlite`, `creek-storage`, …). Without them, every future orchestrator would either fork `creekd` or reimplement substrate. Landing them before v1.0 freeze keeps the supervisor cohesive.

**Out of scope**: this RFC does not specify any orchestrator. It does not pick which primitives ship. It is purely the creekd-side enabling work.

## Status of each extension

| Extension | Required for Phase 2 primitives | Risk | Effort estimate |
|---|---|---|---:|
| 1. Volume mounts | All stateful primitives | Low — bind-mount is well-trodden | 3-5 days |
| 2. TCP dispatch | PG, KV, NATS-as-queue, anything non-HTTP | Medium — design choice between per-app-port and SNI-routing | 1-2 weeks |
| 3. Dep-aware spawn | PG (Patroni→etcd), Queue-on-PG | Low — wait-for-status polling | 2-3 days |
| 4. Snapshot hook | Backup consistency (PG, MinIO) | Low — minimal hook surface, big impact | 2-3 days (v1) |

Total: ~3-5 weeks of focused creekd work to clear the Layer 2 deck. Likely sequenced as four small RFC-tracked PRs rather than one big change.

## Extension 1: Volume mounts

### Problem

`creekd` spawns processes with whatever filesystem the host has. Stateful primitives need disk that survives process restart and is visible at a known path inside the spawned process.

### Proposal

Add `Config.VolumeMounts []VolumeMount` to `internal/supervisor/Config`:

```go
type VolumeMount struct {
    // Source is the absolute path on the host (or relative to the
    // configured VolumeRoot in Supervisor). Caller-managed; creekd
    // does not create the directory.
    Source string

    // Target is the absolute path the process sees. With sandbox /
    // chroot active, this is the path inside the chroot. Without
    // sandbox, this is the host path (bind-mounted in place).
    Target string

    // ReadOnly: true for templates / shared libraries; false for
    // mutable data (the common case for primitives).
    ReadOnly bool
}
```

At spawn time, before `exec`, creekd issues `mount --bind` (Linux) for each entry. On error, the spawn fails cleanly (registered cleanup undoes the partial mounts).

`Supervisor.VolumeRoot string` — optional supervisor-wide knob for "where on disk per-tenant volumes live by default" (e.g. `/var/lib/creekd/volumes`). Callers can pass absolute paths to bypass; the default makes per-tenant subdirectories trivial.

Admin API extension (mirror at the API layer):
```json
"volume_mounts": [
  { "source": "tenants/abc/postgres-data", "target": "/data", "read_only": false }
]
```

Validation:
- Reject `..` in either source or target (path traversal).
- Reject sources outside `VolumeRoot` when `VolumeRoot` is configured (containment).
- Reject targets that conflict with sandbox / chroot paths.

Non-Linux platforms: reject the field at API time with a clear error ("volume_mounts requires Linux + cgroup v2"). Don't silently no-op.

### Open questions

- Quota enforcement: do we apply XFS project quotas or just rely on disk-level monitoring? Defer to first orchestrator that needs it.
- Snapshot integration: see extension 4.
- Backup automation: belongs in Layer 3 of `STRATEGY-primitives.md`, not in creekd.

## Extension 2: Non-HTTP TCP dispatch

### Problem

`creekd`'s dispatch listener forwards HTTP only. Postgres, Valkey, NATS, etc. use protocol-specific TCP framing. A stateful primitive cannot route via dispatch today.

### Three candidate approaches

#### Approach 2a: Per-app port allocation

Each spawn gets a unique host port. Clients connect directly. No dispatch involvement.

**Pros**: simplest; no new code path; matches what tenant clusters already do for app ports.
**Cons**: requires per-app port to be reachable from clients (firewall consideration); 50k port limit per host (not real at our scale).

#### Approach 2b: TCP pass-through dispatch

`creekd` runs a TCP listener on a dispatched port. Routes by leading bytes (PROXY protocol) or by SNI-equivalent identifier in the first frame. Forwards bytes to the right app.

**Pros**: uniform interface across primitives; clients connect to one stable port.
**Cons**: protocol-specific framing for each primitive; complex bootstrap; PROXY protocol assumes upstream support.

#### Approach 2c: Per-app netns + IP

Combine with existing `--net-isolation`: each tenant gets its own IP in a netns. Clients connect to per-tenant IP.

**Pros**: cleanest isolation; existing infrastructure.
**Cons**: depends on `--net-isolation` maturity; per-IP allocation; routing config from outside the netns.

### Recommendation

Start with **2a (per-app port)**. It's the smallest change and is sufficient for ≤50k tenants per host (which is well above our Phase 2 target). Migrate to 2b or 2c if and when port exhaustion becomes plausible — which it won't at our scale.

Concretely: extend `Config` with `Port int` (already exists) + `Protocol string` (new, `"http"` or `"tcp"` — informational, used by routing/health-check layers, not by spawn). HTTP apps continue going through dispatch; TCP apps are reachable directly on their assigned port.

### Open questions

- Should TCP-protocol apps be excluded from the HTTP dispatch route table? Probably yes — avoids accidental misroute. Add a flag.
- Connection-level health check (TCP connect succeeds vs HTTP GET /) — extend `HTTPHealthChecker` interface to a `Checker` interface with implementations: `HTTPChecker`, `TCPConnectChecker`. Trivial change.

## Extension 3: Dependency-aware spawn ordering

### Problem

Patroni needs etcd up before it starts. PgBouncer needs Postgres reachable. Queue needs Postgres. Today every caller has to poll status and time spawns themselves.

### Proposal

Add `Config.DependsOn []string` (app IDs). `creekd` waits for each named app to reach `running` (or `unhealthy` — see below) before spawning the dependent. Timeout configurable via existing supervisor knobs.

Edge cases:
- Cyclic dependencies → reject at spawn time.
- Missing dependency → reject at spawn time (the dependency must already be in the registry).
- Dependency crashes after dependent starts → no rollback; the dependent gets its normal supervised behaviour (health-check fails or process exits, then restart with usual backoff).

`Status` to wait for: `running` is the obvious answer, but Patroni-style "stable enough to dependents" can take 30-60s after `running`. The orchestrator's `HealthCheckPath` (already exists, `5d157c2`) covers the readiness vs liveness distinction. So `DependsOn` waits for `running`; readiness is enforced by `HealthCheckPath` on the dependency.

Admin API:
```json
"depends_on": ["etcd", "patroni-0"]
```

### Open questions

- Cross-tenant dependencies? Reject; dependencies should be intra-tenant (orchestrator's responsibility to scope IDs).
- Hot-restart of a dependency (`creekctl restart etcd`) — do dependents restart? Probably no; the supervisor's failure-then-recover behaviour covers it.

## Extension 4: Snapshot / quiesce hook (optional v1)

### Problem

For consistent backups, some primitives need a callback before/after a snapshot operation. PG's pg_basebackup handles its own consistency. MinIO snapshots can use filesystem-level snapshots (LVM, ZFS) which need quiesce.

### Proposal

Add a simple webhook callout:

```go
type SnapshotHook struct {
    PreSnapshotURL  string  // POST'd before snapshot
    PostSnapshotURL string  // POST'd after snapshot
    TimeoutMS       int     // creekd waits up to this for each
}
```

Orchestrator runs an HTTP server inside the same network space; creekd POSTs to it; orchestrator quiesces (e.g. `pg_start_backup()` for PG, or `mc admin freeze` for MinIO) and returns 200. Post-snapshot POST tells orchestrator to thaw.

### Should this be v1?

Probably not. Phase 2's first primitive (SQLite) needs no quiesce — Litestream's WAL streaming is the consistency model. MinIO's bucket replication doesn't need a quiesce either. PG needs pg_basebackup which is its own consistency layer. The hook is an enabler for future primitives that need filesystem-level snapshots; defer it.

**Recommendation**: write the design here for completeness, do NOT implement until a Layer 4 orchestrator actually needs it.

## Compatibility

All four extensions are **additive**. Existing supervised apps (stateless, HTTP, no dependencies) require no changes. Default field values are zero / nil / empty — same behaviour as today.

Wire-compatibility with `internal/state/store.go` persistence:
- `Config.VolumeMounts` is a new slice field; absent in old state.json entries means "no mounts" (same as today).
- `Config.DependsOn` same shape.
- `Protocol` defaults to `"http"`.

Test surface:
- Unit tests in `internal/supervisor/` for field-level injection.
- Integration tests (Linux-only) for actual bind-mount + dep-wait.
- E2E test: spawn an etcd, then a Patroni against it, validate dep-order is enforced.

## Timeline

Aim for all four to land before v1.0 API freeze. Rough sequencing:

1. **Volume mounts** — first because it unlocks SQLite (the simplest Layer 4 primitive). ~1 week including tests.
2. **Dep-aware spawn** — second, small. ~3 days.
3. **TCP dispatch (approach 2a)** — third, needed before Postgres or KV land. ~1 week.
4. **Snapshot hook** — design only this time. Skip implementation until first user.

Total: ~2-3 weeks of focused work. Each as a separate atomic PR with its own tests.

## Anti-goals

- **Do not bake any primitive-specific logic into creekd.** No "if this is a Postgres" branches. The extensions stay generic.
- **Do not couple to a specific backup target.** S3-compatible is the obvious default, but creekd itself doesn't push backups; it provides the hook (extension 4) that orchestrators use.
- **Do not promote `creek deploy --db=creekd` until trigger conditions are met** (per `STRATEGY-primitives.md`). The substrate extensions enable the orchestrator path, but the orchestrator does not exist yet.

## See also

- `ARCHITECTURE.md` (principles 1 + 2) — stdlib-first and substrate-not-policy commitments that constrain this RFC
- `docs/DESIGN.md` — existing engineering design context (process model, cgroup, dispatch)
- `STRATEGY-primitives.md` (private planning repo) — the Layer 2 / Layer 3 / Layer 4 framework these extensions slot into
- `DESIGN-creekdb.md` (private planning repo) — first concrete consumer of all four extensions
