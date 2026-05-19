# creekd vs pm2 — head-to-head

Same machine, same toy app, fair-as-possible measurement. The honest answer to "is this really better than pm2?"

## Measured (this machine)

```
darwin/amd64, Intel i9-10910 (3.6 GHz), 64 GB RAM
go 1.22, creekd HEAD, pm2 7.0.1, node v22.15.0
N=20 samples per scenario
```

| Metric | creekd | pm2 | ratio |
|---|---:|---:|---:|
| Spawn → /healthz 200 (p50) | **22.2 ms** | 186.7 ms | pm2 **8.4×** slower |
| Spawn → /healthz 200 (p95) | **24.4 ms** | 212.1 ms | pm2 **8.7×** slower |
| Supervisor RSS (idle) | **12 MB** | 60 MB | pm2 **4.9×** heavier |

Reproduce: `./up.sh && go run ./bench -n 20`.

## Cited (from pm2 source)

| Behaviour | creekd | pm2 | source |
|---|---|---|---|
| Memory cap enforcement | Kernel OOM via cgroup v2 memory.max | Poll-based — checks every 30 s | `pm2/constants.js: WORKER_INTERVAL = 30000` (ms) — see `lib/Worker.js:79` for the check |
| Memory cap reaction time | < 100 ms (kernel ~ms) | up to 30 s (worst case), ~15 s median | derived from poll interval above |
| Routing | Built-in HTTP dispatch (host/header) | Not included — needs nginx / Caddy | pm2 has no router |
| Linux namespace isolation | Opt-in (PID/UTS/IPC/Mount/User) | None | pm2 design |

## Pros / cons

### Where creekd is better

- **Real memory cap.** cgroup v2 hands enforcement to the kernel. pm2's `max_memory_restart` is a 30-second polling timer in JS — by the time pm2 notices, a memory leak has already pushed swap or pushed out the page cache. Worst-case latency: 300× longer.
- **Spawn cost.** Direct `clone3 + exec`, no JS interpreter on the supervisor side. Measured 8× faster end-to-end including `creekctl` itself.
- **Supervisor footprint.** ~12 MB vs ~60 MB. Matters when running a dense fleet on a small VPS.
- **Routing in the box.** No second daemon, no nginx config reload dance when an app's port changes.
- **Real isolation.** PID / mount / user / UTS / IPC namespaces, chroot, NoNewPrivs. pm2 has none of this.
- **Single static binary.** No Node runtime, no npm install at startup.

### Where pm2 is better

- **Maturity.** Released 2014. Battle-tested at scale, ecosystem of plugins, dozens of stack-specific recipes.
- **Windows.** pm2 runs on Windows; creekd is Linux + macOS-dev only.
- **In-process cluster mode.** `pm2 start app.js -i max` forks N Node workers behind one shared port via Node's cluster module. creekd has dispatch routing but not in-process worker pooling.
- **Node-specific log management.** Built-in log merging, rotation, timestamps that are aware of Node's quirks. creekd's logs are simpler.
- **Hot reload / file watching.** `pm2 start app.js --watch`. creekd has nothing equivalent (and won't — that's the runtime layer's job).
- **`pm2 monit` interactive dashboard, `pm2.io` SaaS.** No creekd analogue.

### Where they're rough equivalents

- Restart-on-crash. Both do exponential backoff. creekd's policy is documented in `internal/supervisor/restart.go`; pm2's is in `lib/God.js`.
- Persisted app list across daemon restarts. creekd has `state.json` (atomic write). pm2 has `pm2 save` + `pm2 resurrect` (must be invoked manually).
- Graceful shutdown via SIGTERM → SIGKILL.

## Methodology

The bench in `bench/main.go`:

1. Builds nothing — assumes `./up.sh` has already produced `bin/creekctl` and `bin/toy`. Both binaries are stable Go programs; no JIT warmup confusion.
2. **Spawn time**: `creekctl up <id>` / `pm2 start ./bin/toy --name <id>` is timed end-to-end. The clock stops on the first 200 from `/healthz`. Each sample uses a fresh `<id>` so neither supervisor benefits from cache. Between samples the app is `rm`'d.
3. **RSS**: `ps axo rss` on the supervisor process. For pm2 we read the "PM2 v..." daemon row. The supervisor is idle (no apps) at the read.
4. Each scenario runs N=20 by default. Numbers reported: p50, p95, min, max.

### What the bench doesn't show

- **Memory cap latency** — needs Linux + cgroup write access. macOS dev hosts can't run cgroup. The 300× headline is derived from public source (kernel OOM vs `WORKER_INTERVAL=30000`), not measured here. If you want to measure it: `make test-linux` inside this repo exercises `TestMemoryLimitTriggersOOMKill` which times the creekd side at < 50 ms; the pm2 side is bounded below by 30 s by construction.
- **Routing latency.** Not measured because pm2 doesn't have a router. A fair comparison would add nginx in front of pm2, which adds another hop and another config surface — making the comparison less apples-to-apples, not more.
- **Memory under load.** This is an idle-state RSS comparison. Both will grow under traffic.

## When to pick which

**Stay with pm2 when:**
- You're on Windows.
- You want a turnkey Node.js dev tool with file-watching, in-process cluster, and a maintained dashboard.
- Your apps are exclusively Node and you value pm2-specific features (cluster mode, ecosystem) over isolation.

**Switch to creekd when:**
- Your memory cap actually has to be a cap. If you've ever been bitten by a leaky Node process eating the box for 25 seconds before pm2 cycled it, you know.
- You want one binary to do supervision *and* routing, with no nginx dance.
- You're on Linux and want cgroup / namespace isolation between apps.
- You're mixing runtimes (Bun + Node + Deno + plain binaries).
- The supervisor itself's footprint matters — dense hosting, embedded, edge.
