# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog 1.1.0](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Pre-1.0 releases may carry breaking changes in `MINOR` bumps; this is called out in each release's notes when it happens.

## [Unreleased]

### Added

- `creekctl up --from <manifest.json>` reads a manifest written by [`@solcreek/adapter-creekd`](https://github.com/solcreek/adapter-creekd) and seeds the spawn request (runtime, entrypoint, port). CLI flags retain priority — any value the user passes on the command line overrides the corresponding manifest field. Validates manifest version, target, framework, runtime, and port range; rejects malformed JSON with a clear error. Entrypoint is rejected if absolute or containing `..` traversal.
- `creekctl deploy --from <manifest.json>` — symmetric counterpart for blue-green redeploy from an updated manifest. Same CLI flag precedence as `up --from`, same three fields seeded (runtime, entrypoint, port). Closes the adapter manifest's continuous-deploy loop: rebuild → manifest updates → `deploy --from` pushes the new artifact.
- `examples/nextjs-density/` — Next.js idle RAM density bench vs `docker run`. Measured Linux numbers (Hetzner cx33, Bun 1.3.14): 1.45× per-app PSS overhead for docker; 1.63× total kernel memory; 45× faster bare-bun spawn for N=50.
- `examples/stack-density/` — per-app idle PSS across 5 stacks (Bun raw, Hono, SvelteKit, Astro, Next.js). The lightest stack fits ~5× more apps per host than the heaviest. Bash-only harness, Linux-only (uses `/proc/<pid>/smaps_rollup`).

### Fixed

- `creekd --version` / `-v` / `version` now print the build-time version and exit 0 before the daemon initialises. Previously fell through to daemon startup, bound the dispatch + admin ports, and hung any command substitution (notably `install.sh`'s post-install version display).
- `supervisor.Spawn` now validates the app ID itself via `ValidateID` before any side effects. External callers (admin API, state restore) already validated upstream; this closes the gap if a future caller forgets. Deploy's internal blue-green spawn uses `spawnUnchecked` because its `deployTempID` deliberately fails the grammar.
- `state.Store` AddApp/RemoveApp use copy-on-write semantics: build a candidate map, flush to disk, swap in-memory only on flush success. A failed flush no longer leaks into the in-memory cache where a later successful flush would silently persist the supposedly-failed change.
- `state.Store` deep-copies `supervisor.Config` on store and retrieve. Args/Env slices and CgroupLimits/Sandbox pointer targets are no longer aliased between caller and persisted snapshot.
- `creekctl up --from` / `deploy --from` reject manifest entrypoints that are absolute paths or contain `..` traversal. Defense in depth — currently runs under a local trust model, but the validation removes the requirement for a second layer later if a hosted control plane ever consumes customer manifests.

### Changed

- Several internal `doc.go` package docs rewritten to match implementation: `runtime` (Detect inspects file fingerprints, not source imports), `dispatch` (stdlib httputil.ReverseProxy, no Caddy embed, no health-gated routing), `cgroup` (non-Linux returns ErrUnsupported and fails the spawn, doesn't silently degrade), `state` (cloneMap comment now accurately describes the aliasing rule).

## [0.1.1] - 2026-05-19

Patch release within 24 hours of `v0.1.0`, addressing six issues surfaced by an external code review. Five of them were correctness bugs; the sixth was documentation drift. Every fix has a covering regression test.

### Fixed

- **`--net-isolation` was shipped broken in `v0.1.0`** across three independent layers:
  - `cmd/creekd` now reads `CREEKD_NET_SUBNET` and `CREEKD_NET_BRIDGE_NAME` env vars. Without these the supervisor fields stayed empty and every net-isolated spawn failed with `"NetIsolation requires both NetSubnet and NetBridgeName"`.
  - `HTTPHealthChecker` now probes `app.NetIP` when the app is net-isolated. The hardcoded `http://127.0.0.1:<port>` could not reach a netns'd listener, so the supervisor health-loop would silently mark every net-isolated app unhealthy and restart-loop it.
  - `Supervisor.Deploy` now uses `router.SetAddr` with `v2.NetIP`. The prior `router.Set` defaulted host to `127.0.0.1`, breaking traffic to any newly-deployed net-isolated app.
- **`creekctl reset` no longer silently drops the app's `Env`.** Crash-loop recovery was starting the new process with an empty environment — `DATABASE_URL`, `AUTH_TOKEN`, custom `PORT`, etc. would vanish without warning. The supervisor now persists `Env` on the `App` struct at `Spawn` time and threads it through every restart path.
- **Admin API validates app ID grammar** (`^[a-z0-9][a-z0-9-]{0,62}$`) before the ID becomes a log directory, cgroup slice element, netns name, or state-file key. Defense in depth against path traversal, separators, shell metacharacters, null bytes, etc. — the admin listener is loopback + token-gated by default, but the cheap fix doesn't pay for itself to skip.
- **Dispatch router's `Backend.ErrorHandler` is set once at construction** instead of per request. The prior per-request mutation was a data race on a shared `*httputil.ReverseProxy` field; even though the closure content was identical, the Go memory model treats it as racey.

### Changed

- `docs/DESIGN.md` no longer claims hostname-based dispatch routing. The implementation only reads the `X-Creek-App` header (or the `?app=` query fallback); hostname mapping requires a front-door like Caddy that copies the relevant `Host` substring into `X-Creek-App`.
- `internal/supervisor/supervisor.go` package comment was stuck at M5.1 ("naive immediate restart"). Rewritten to reflect the current scope (restart policy, health probing, deploy, cgroup, sandbox, netns, log capture).
- Inline `prctl(PR_SET_NO_NEW_PRIVS)` reframed honestly as Phase 2 work bundled with seccomp + capability-drop, all of which need CGO and therefore land together. The prior doc note read as an open todo with no owner.

### Added

- `examples/bun-app/` — first example exercising the `--runtime bun --entry server.ts` path. Uses Bun-native features (`Bun.serve`, `bun:sqlite`, SSE streaming through the dispatch reverse proxy) so swapping in Node would actually fail.
- `examples/README.md` — index page with a scannable table of all runnable recipes (four rows at this release; more added in later versions).
- `TestConfigureSupervisorFromEnv` — pins all four `CREEKD_*` env vars used by the daemon. This is the test that should have existed in `v0.1.0` to catch the net-iso gap.
- `TestSpawnRejectsInvalidID` — admin-API-level coverage for the new ID grammar.
- `TestResetPreservesEnv` — marker-file roundtrip through crash-loop + Reset.
- `TestNetIsolationHealthProbeReachesContainer` and `TestNetIsolationDeployRoutesViaNetIP` — privileged-Linux integration tests for the two net-iso correctness fixes.
- `TestBackendDownConcurrentServeIsRaceFree` — 32 concurrent goroutines through a dead backend with the race detector on, guarding the `ErrorHandler` regression.

## [0.1.0] - 2026-05-19

First public release. The supervisor is now installable via `curl install.sh | sh`, runnable as a daemon, and bench-comparable against existing tools on the same machine.

### Added

- **Supervisor core** — child-process spawn / supervision / restart-policy (exponential backoff + crash-loop detection) / graceful shutdown / health probing / blue-green deploy.
- **HTTP dispatch** — single listener routes by `X-Creek-App` header (or `?app=` query fallback) through Go's stdlib reverse proxy. No nginx required.
- **Multi-runtime** — `--runtime bun|node|deno --entry <path>` resolves to the canonical invocation. Explicit `--command + --args` mode also supported.
- **cgroup v2 enforcement** — `memory.max` (with `memory.swap.max=0` so the cap is real), `pids.max`, `cpu.max`. Hard memory caps trigger kernel OOM in `<100 ms`, vs. `pm2`'s 30 s polling timer.
- **Linux sandbox** — opt-in PID / UTS / IPC / mount / user namespaces, chroot, NoNewPrivs via `setpriv` wrap. Composable; zero values mean host-shared.
- **Per-app network namespace** — bridge + veth + iptables masquerade. Each app gets its own private subnet IP.
- **Admin HTTP/JSON API** — `POST /v1/apps` (spawn), `DELETE /v1/apps/{id}` (stop), `POST /v1/apps/{id}/{deploy,restart,reset}`, `GET /v1/apps[/{id}[/{stats,logs}]]`. Bearer-token auth; hard-required when listener is non-loopback.
- **`creekctl`** — admin CLI mirroring the API surface (`ps`, `get`, `up`, `rm`, `restart`, `reset`, `deploy`, `logs`, `stats`).
- **State persistence** — `state.json` with atomic-rename writes; daemon restart replays every declared app before opening listeners.
- **Per-app log capture** — size-based rotation under `<CREEKD_LOG_DIR>/<id>/`.
- **`/debug/pprof/*` endpoints** — opt-in via `CREEKD_DEBUG_PPROF=1`, gated by the same admin token.

### Examples and benchmarks

- `examples/pm2-replacement/` with `COMPARISON.md` — measured 8.4× faster spawn, 4.9× leaner supervisor RSS than `pm2`, plus the kernel-OOM (`<100 ms`) vs. poll-based (`WORKER_INTERVAL=30000` ms) memory-cap reaction-time delta cited from `pm2` source.
- `examples/sandboxed-eval/` with `COMPARISON.md` — measured 2.6× faster cold spawn than `docker run` for matched cgroup + namespace + no-new-privs. Honest pros/cons including where docker still wins (seccomp + cap-drop defaults).
- `examples/review-apps/` — side-by-side preview environments with a `creekctl deploy`-based zero-downtime swap.

### Release infrastructure

- `goreleaser` configuration cross-compiles `creekd + creekctl` for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`. Zero CGO in Phase 1.
- GitHub Actions release workflow fires on `v*` tag push.
- Production `Dockerfile` (multi-stage, debian-slim runtime, util-linux + iproute2 + iptables for the sandbox/netns features).
- `install.sh` — POSIX shell installer with OS / arch detection, SHA-256 verification, and root-vs-user prefix logic.
- `docs/CONFIG.md`, `docs/DESIGN.md`, `SECURITY.md`, `CONTRIBUTING.md` published alongside the binaries.

### Known limitations (carried forward)

- `--no-new-privs` + `--chroot` don't compose unless `setpriv` is inside the rootfs. Lands cleanly in Phase 2 alongside seccomp.
- No seccomp, no capability drop. Phase 2.
- No supervisor-survive-restart reattach (apps die when creekd dies, declarations survive, restart replays). Phase 2.
- Single host. No clustering, no multi-host scheduling.
- Log retention is size-only, no time-based rotation, no remote shipping.

[Unreleased]: https://github.com/solcreek/creekd/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/solcreek/creekd/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/solcreek/creekd/releases/tag/v0.1.0
