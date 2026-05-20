# creekd

[![ci](https://github.com/solcreek/creekd/actions/workflows/ci.yml/badge.svg)](https://github.com/solcreek/creekd/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/solcreek/creekd.svg)](https://pkg.go.dev/github.com/solcreek/creekd)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> Multi-tenant process supervisor and HTTP dispatcher. Cgroup v2 + Linux namespace isolation. Single Go binary.

`creekd` is the host-side daemon that runs a fleet of independent application processes on one machine and routes HTTP traffic to them. Each app is its own child process, confined by cgroup v2 limits and (optionally) Linux namespaces. The supervisor handles spawn, restart, health, log capture, and zero-downtime blue-green deploys.

It is the runtime substrate of the [Creek](https://github.com/solcreek/creek) platform, but it has no Creek-specific dependencies — `creekd` will host any process that listens on a TCP port.

## Status

**Phase 1, pre-release.** API and CLI surfaces are still in flux. Not production-ready. See [`docs/ROADMAP.md`](docs/ROADMAP.md) for what's planned and [`CHANGELOG.md`](CHANGELOG.md) for what's shipped.

## Why it exists

Most "run my process and route traffic to it" systems force a choice: heavyweight (Kubernetes, Nomad, full container runtimes) or single-tenant (systemd, supervisord, pm2). `creekd` is the middle: multi-tenant on one host, no container daemon, no scheduler, no overlay network — just a Go binary that owns child processes and the listening socket.

Trade: you give up multi-host orchestration. You get a 10 MB binary that runs hundreds of isolated apps on a $30 VPS.

## When creekd makes sense

You already build and ship binaries (Bun / Node / Deno or any process that listens on a port), and:

- you want multiple of them on one box with real isolation between them
- you want pm2-level operational simplicity but with cgroup v2 + namespace teeth
- you'd rather own the supervisor than rent it
- you're fine running one machine — multi-host is solved one layer up (a load balancer in front of N creekd hosts)

See [`examples/`](examples/README.md) for six runnable recipes covering supervisor replacement, sandboxed code runner, branch review apps, Bun framework demo, Next.js density vs `docker run`, and idle-PSS per stack (Bun / Hono / SvelteKit / Astro / Next.js). Most carry head-to-head benches with reproducible numbers.

## When it doesn't (yet)

- **You need `git push` deploys.** No build pipeline in `creekd`; that lives in the wider Creek stack.
- **You need TLS in the daemon.** Put Caddy / Cloudflare / nginx in front.
- **You need a web dashboard.** Only CLI (`creekctl`) and JSON API right now.
- **You need multi-host scheduling.** One host per `creekd`; no clustering.
- **You're running hostile multi-tenant workloads.** Phase 1 sandbox (cgroup + namespaces + chroot + NoNewPrivs) is meaningful but not full — seccomp and capability drop land in Phase 2.

## Install

Released binaries for linux + darwin × amd64 + arm64:

```bash
curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/install.sh | sh
```

Or build from source — requires Go 1.22+, Linux for the full feature set, macOS for dev (cgroup / namespace paths self-skip):

```bash
go build -o bin/creekd  ./cmd/creekd
go build -o bin/creekctl ./cmd/creekctl
```

## Quickstart

```bash
# Run the daemon (loopback only, no auth — dev mode).
creekd &

# In another shell: spawn an app.
creekctl up hello \
    --command /bin/sh \
    --arg -c \
    --arg 'while true; do printf "HTTP/1.1 200 OK\r\nContent-Length: 5\r\n\r\nhello" | nc -l -p 18000 -q 1; done' \
    --port 18000

# Route to it via the dispatch listener.
curl -H 'X-Creek-App: hello' http://127.0.0.1:9000/
# => hello

# Inspect, then stop.
creekctl ps
creekctl rm hello
```

For real apps see [`docs/CONFIG.md`](docs/CONFIG.md) and the runtime profiles in [`internal/runtime/`](internal/runtime).

## Architecture

```
                       creekd
   ┌──────────────────────────────────────────┐
   │                                          │
   │  admin api    ◀──── HTTP/JSON ────  creekctl
   │  127.0.0.1:9080     (bearer auth)
   │  ├─ spawn / stop / restart / deploy
   │  ├─ ps / get / logs / stats
   │  └─ /debug/pprof  (opt-in)
   │                                          │
   │  dispatch    ◀──── HTTP ──────────  end-users
   │  :9000              (host/header → app)
   │                                          │
   │  supervisor                              │
   │   ┌─────┐  ┌─────┐  ┌─────┐              │
   │   │ app │  │ app │  │ app │   ← child processes
   │   │  A  │  │  B  │  │  C  │     each w/ own cgroup,
   │   │ Bun │  │Node │  │Deno │     optional netns/PID ns,
   │   └─────┘  └─────┘  └─────┘     setpriv NoNewPrivs
   │                                          │
   └──────────────────────────────────────────┘

       state ──────► state.json   (declared apps, atomic write)
       logs  ──────► <log-dir>/<id>/*.log  (size-rotated)
```

Control plane and data plane are **separate listeners** so admin tooling can sit behind one auth boundary while end-user traffic goes through another. The dispatch listener is also independently disable-able for admin-only deployments.

See [`docs/DESIGN.md`](docs/DESIGN.md) for design rationale and [`ARCHITECTURE.md`](ARCHITECTURE.md) for the principles that govern what gets added.

## Configuration

Everything is environment-variable driven. Full reference: [`docs/CONFIG.md`](docs/CONFIG.md).

The essentials:

| Variable | Default | Purpose |
|---|---|---|
| `CREEKD_ADMIN_ADDR` | `127.0.0.1:9080` | Admin / control plane listener |
| `CREEKD_ADMIN_TOKEN` | _empty_ | Bearer token; **required** for non-loopback admin |
| `CREEKD_DISPATCH_ADDR` | `127.0.0.1:9000` | Public dispatch / data plane (empty disables) |
| `CREEKD_LOG_DIR` | _empty_ | Per-app log capture root |
| `CREEKD_CGROUP_PARENT` | _empty_ | Cgroup v2 slice for per-app sub-cgroups |
| `CREEKD_STATE_DIR` | _empty_ | Directory holding `state.json` (declared-app persistence) |
| `CREEKD_NET_SUBNET` / `CREEKD_NET_BRIDGE_NAME` | _empty_ | Per-app netns subnet (e.g. `10.42.0.0/24`) + bridge name. Both required for `--net-isolation` |
| `CREEKD_DEBUG_PPROF` | _unset_ | Set to `1` to mount `/debug/pprof` |

## Building & testing

```bash
go build ./...                                  # build everything
go test  -race ./...                            # full unit suite

make test-linux                                 # privileged Linux suite
                                                # (cgroup v2, netns, namespaces)
                                                # runs inside Docker; works
                                                # on macOS hosts

make bench                                      # benchmark smoke
make bench-cpu                                  # + cpu/mem profiles
```

CI runs all three (`test`, `test-linux-privileged`, `bench`) on every push and PR.

## Project layout

```
creekd/
├── cmd/
│   ├── creekd/           # daemon entry point
│   └── creekctl/         # admin CLI (talks to the admin API)
├── internal/
│   ├── supervisor/       # child-process lifecycle, restart policy
│   ├── runtime/          # Bun / Node / Deno auto-detection
│   ├── cgroup/           # cgroup v2 (memory / pids / cpu)
│   ├── sandbox/          # Linux namespaces, chroot, NoNewPrivs
│   ├── network/          # per-app netns + veth + iptables
│   ├── dispatch/         # HTTP router, host → app
│   ├── deploy/           # blue-green zero-downtime swap
│   ├── logs/             # capture + size-based rotation
│   ├── state/            # state.json persistence (atomic rename)
│   ├── adminapi/         # HTTP/JSON control plane
│   └── adminclient/      # typed Go client for the admin API
├── docs/                 # roadmap, design, config
└── .github/workflows/    # CI
```

## Related

- [`solcreek/creek`](https://github.com/solcreek/creek) — runtime libraries, CLI for end developers, examples (TypeScript)

## License

Apache 2.0. See [LICENSE](LICENSE).
