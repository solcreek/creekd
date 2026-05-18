# creekd

> Process supervisor and HTTP dispatcher for the Creek runtime.

`creekd` is the host-side daemon that supervises Creek applications. Each app runs as an isolated child process; `creekd` handles spawning, restart policy, health probes, cgroup enforcement, log capture, and zero-downtime blue-green deploys.

This repo holds **only** the daemon. The runtime libraries, CLI, and bindings live in the main Creek monorepo.

## Status

**Phase 1 work in progress.** Not production-ready. See [`docs/ROADMAP.md`](docs/ROADMAP.md) for what's planned.

## Why a separate repo

- Different language (Go) from the runtime libraries (TypeScript)
- Different release cadence (supervisor should be stable; runtime iterates)
- Different audience (sysadmins, not application developers)
- Single-binary distribution friendly (`curl install.creek.dev/creekd | sh`)

The boundary is enforced by the gRPC protocol — see [`api/proto/`](api/proto/) for the wire schema.

## Architecture

```
                    creekd (supervisor)
                   ┌──────────────────────────┐
       gRPC ──────→│ admin API                │
       (CLI)       │ ├─ deploy / undeploy     │
                   │ ├─ ps / logs / restart   │
                   │ └─ status                │
                   ├──────────────────────────┤
       HTTP ──────→│ dispatch (Caddy-backed)  │
       (traffic)   │ routes by X-Creek-App    │
                   ├──────────────────────────┤
                   │ supervisor goroutines    │
                   │   ┌─────┐  ┌─────┐  ...  │
                   │   │ app │  │ app │       │
                   │   │  A  │  │  B  │       │
                   │   │ Bun │  │Node │       │
                   │   └─────┘  └─────┘       │
                   │   (child processes)      │
                   │   each cgroup-bounded    │
                   └──────────────────────────┘
```

## Building

Requires Go 1.22+.

```bash
# build
go build -o bin/creekd ./cmd/creekd

# run
./bin/creekd

# test
go test ./...

# lint (after installing golangci-lint)
golangci-lint run
```

## Running

```bash
# Single-app default port
./bin/creekd

# Custom port + apps directory
CREEK_PORT=8080 CREEK_APPS_DIR=/var/lib/creekd ./bin/creekd
```

Configuration is environment-variable driven (12-factor); see [`docs/CONFIG.md`](docs/CONFIG.md) for the full list (TBD).

## Project layout

```
creekd/
├── cmd/
│   └── creekd/            # main binary entry point
├── internal/
│   ├── supervisor/        # child-process lifecycle (M5.1-M5.3)
│   ├── runtime/           # Bun/Node/Deno dispatch (M5.4)
│   ├── cgroup/            # cgroup v2 enforcement (M5.5)
│   ├── logs/              # log capture, rotation, JSON (M5.6)
│   ├── deploy/            # blue-green deploy (M5.7)
│   └── dispatch/          # HTTP routing layer
├── api/
│   └── proto/             # gRPC schema shared with creek CLI
├── docs/                  # detailed docs (roadmap, config, ops)
└── pkg/                   # public Go SDK (when needed)
```

## License

Apache 2.0. See [LICENSE](LICENSE).

## Related

- [solcreek/creek](https://github.com/solcreek/creek) — runtime, host-runtime, CLI, examples (TypeScript monorepo)
- Project strategy and roadmap: see private `product-planning/` archive
