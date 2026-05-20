# Configuration

`creekd` is configured entirely through environment variables — no config file. Each variable is independent; unset variables fall back to the documented default.

## Listeners

### `CREEKD_ADMIN_ADDR`

Listen address for the admin HTTP/JSON API (control plane).

- **Default**: `127.0.0.1:9080`
- **Format**: `host:port`

This is the API that `creekctl` and any operator tooling talks to. Operations include spawn, stop, deploy, restart, logs, and stats.

### `CREEKD_ADMIN_TOKEN`

Bearer token required on every admin request.

- **Default**: empty (no auth)
- **Hard requirement** when the admin listener is bound to a non-loopback address. If the listener is not loopback and the token is empty, creekd refuses to start.

Clients send the token in the `Authorization: Bearer <token>` header.

### `CREEKD_DISPATCH_ADDR`

Listen address for the public dispatch proxy (data plane). This is where end-user HTTP traffic arrives; the router forwards each request to the right app process based on the `X-Creek-App` header (or `?app=<id>` query parameter as a fallback for clients that can't set headers).

- **Default**: `127.0.0.1:9000`
- **Format**: `host:port`
- **Empty** (`CREEKD_DISPATCH_ADDR=`) disables the dispatch listener. Useful for admin-only deployments.

## App runtime

### `CREEKD_LOG_DIR`

Per-app log capture root. When set, each app's stdout / stderr is captured to `<dir>/<app-id>/` with size-based rotation. When unset, child output is forwarded to creekd's own stdout / stderr (test / dev mode).

- **Default**: empty
- **Recommended in production**: yes — without it there is no log retention.

### `CREEKD_CGROUP_PARENT`

Name of the cgroup v2 slice that owns per-app sub-cgroups. Required for any per-app resource enforcement (memory, pids, cpu).

- **Default**: empty
- **Example**: `creekd.slice`
- **Requires**: Linux, cgroup v2, and creekd running with permission to write under the parent slice (typically root, or `Delegate=yes` in a systemd unit).
- **Empty** disables cgroup enforcement — apps run with the same limits as creekd itself.

### `CREEKD_DEFAULT_MEMORY_HIGH`

Daemon-wide floor for cgroup `memory.high` — the **soft** memory cap that throttles allocations (no OOM-kill). Applied to every app whose `creekctl up` did not pass an explicit `--memory-high`, so noisy-neighbor protection is opt-out rather than opt-in.

- **Default**: empty (no daemon-wide default; only explicit `--memory-high` enforces a soft cap)
- **Recommended**: `256M` — see [`examples/cgroup-memory-tuning/RESULTS.md`](../examples/cgroup-memory-tuning/RESULTS.md) for the experiment that justifies this value (false-positive sweep across five stacks, containment behaviour, sibling-impact measurement).
- **Format**: integer with optional `K`/`M`/`G`/`T` (binary, ×1024). `256M` = `256Mi` = `256MiB` = `268435456`.
- **Requires**: `CREEKD_CGROUP_PARENT` set. Without a parent slice there's nowhere to install the limit; the daemon refuses to silently ignore the knob.
- **Per-app override**: `creekctl up --memory-high <size>` always wins. The env var sets the floor, not the ceiling.
- **Malformed values** (e.g. typos) fail daemon startup rather than silently disabling protection.

### `CREEKD_DEFAULT_MEMORY_MAX`

Daemon-wide floor for cgroup `memory.max` — the **hard** memory cap that triggers a cgroup-scoped OOM-kill when crossed. Pairs with `CREEKD_DEFAULT_MEMORY_HIGH` as the safety net for the rare case where the soft cap can't keep up.

- **Default**: empty (no daemon-wide default)
- **Recommended**: `1G` — see [`examples/cgroup-memory-tuning/RESULTS.md`](../examples/cgroup-memory-tuning/RESULTS.md) Phases 4-6. Empirically, memory.high alone contains every realistic JS-runtime leaker pattern at ~278 MB; memory.max never fires below ~1G even under adversarial allocation. The hard cap is insurance against pathological cases the experiment couldn't construct.
- **Format**: same as `CREEKD_DEFAULT_MEMORY_HIGH`.
- **Requires**: `CREEKD_CGROUP_PARENT` set.
- **Per-app override**: `creekctl up --memory-max <size>` always wins.
- **Malformed values** fail daemon startup.

### `CREEKD_STATE_DIR`

Directory holding `state.json`, the persisted set of declared apps.

- **Default**: empty (no persistence — apps vanish when creekd restarts)
- **When set**: creekd loads `<dir>/state.json` at startup and replays every recorded app through `Spawn` before opening listeners. Spawn / deploy / stop operations write through to the file atomically.
- **Semantics**: declarations persist, processes do not. After a creekd restart the supervisor re-spawns fresh processes from the saved configs.

## Network isolation

Per-app network namespace requires **both** of the following. Either-one-set is rejected at spawn time. Both empty disables `--net-isolation` entirely (apps share the host network, dispatch routes directly to `127.0.0.1:<port>`).

### `CREEKD_NET_SUBNET`

IPv4 CIDR carved up among per-app namespaces.

- **Default**: empty (net isolation disabled)
- **Example**: `10.42.0.0/24` — gives ~250 simultaneously-isolated apps
- **Requires**: Linux + privileged daemon (creating bridges + veth pairs + iptables rules).

### `CREEKD_NET_BRIDGE_NAME`

Name of the host-side bridge interface that veth pairs attach to. Created on first net-iso spawn; reused thereafter.

- **Default**: empty
- **Example**: `creekbr0`
- **Constraint**: max 15 chars (Linux `IFNAMSIZ`). Avoid names that collide with existing interfaces.

## Operations

### `CREEKD_DEBUG_PPROF`

Mounts `/debug/pprof/*` on the admin listener.

- **Default**: unset (off)
- **Set to `1`** to enable.
- **Auth**: the same `CREEKD_ADMIN_TOKEN` gates the pprof endpoints.

Useful for live CPU / heap / goroutine profiling. Off by default because exposing pprof on production listeners is a known DoS / info-disclosure surface.

## Example: production-ish

```bash
export CREEKD_ADMIN_ADDR=0.0.0.0:9080
export CREEKD_ADMIN_TOKEN="$(openssl rand -hex 32)"
export CREEKD_DISPATCH_ADDR=0.0.0.0:80
export CREEKD_LOG_DIR=/var/lib/creekd/logs
export CREEKD_CGROUP_PARENT=creekd.slice
export CREEKD_DEFAULT_MEMORY_HIGH=256M
export CREEKD_DEFAULT_MEMORY_MAX=1G
export CREEKD_STATE_DIR=/var/lib/creekd

# Optional: enable per-app network namespaces. Drop these for
# shared-network mode.
export CREEKD_NET_SUBNET=10.42.0.0/24
export CREEKD_NET_BRIDGE_NAME=creekbr0
creekd
```

## Example: local dev

```bash
# Loopback-only admin without a token; in-process logs; no cgroup.
creekd
```

This is equivalent to all defaults: admin on `127.0.0.1:9080`, dispatch on `127.0.0.1:9000`, no log files, no cgroup enforcement, no persistence.
