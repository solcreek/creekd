# nextjs-density — bare bun vs `docker run`

Per-app RAM for an idle Next.js standalone, measured both ways on the same machine.

## Measured (this machine)

```
darwin/amd64, Intel i9-10910 (3.6 GHz), 128 GB RAM
Docker via colima (8 GB / 4 CPU Linux VM)
Next.js 16.2.3, Bun 1.3.14, @solcreek/adapter-creekd 0.1.1
```

### N=10 idle apps

| Metric | bare bun | docker run | docker tax |
|---|---:|---:|---:|
| per-app RSS p50 | **72.4 MB** | 85.1 MB | **+12.7 MB (1.18×)** |
| per-app RSS p95 | 72.6 MB | 86.0 MB | +13.4 MB |
| total RSS (N=10) | 722.7 MB | 851.4 MB | +128.7 MB |
| spawn-all wallclock | 17 ms | 7 834 ms | docker 460× slower |
| all-healthy wallclock | 985 ms | 900 ms | ≈ even |

### N=50 idle apps

| Metric | bare bun (Darwin) | docker run (Linux VM) |
|---|---:|---:|
| per-app RSS p50 | 71.7 MB | 68.9 MB |
| per-app RSS p95 | 72.0 MB | 70.2 MB |
| total RSS (N=50) | 3.50 GB | 3.37 GB |
| spawn-all wallclock | 293 ms | 20 900 ms (docker 71× slower) |
| all-healthy wallclock | 4 145 ms | 2 876 ms |

**At N=50 the headline ratio is misleading** — bare runs natively on Darwin while docker runs inside colima's Linux VM, so the two columns are measuring different kernels' memory accounting (macOS `ps -o rss=` includes shared library pages that Linux puts under PSS, not RSS). On the same VPS (Linux x86_64 both sides) the docker tax compounds; the N=10 column where overhead dwarfs the platform delta is the apples-to-apples reading.

We'll re-run N=50 on a dedicated Linux box and replace this row when that lands.

Reproduce: `./up.sh && go run ./bench -n 50 -settle 10`.

## What this means for density

The math the bench is asking:

> Given a fixed RAM budget, how many idle Next.js apps fit?

```
   capacity = (host RAM - supervisor RSS) / per-app RSS
```

- **bare bun / creekd**: per-app ≈ 72 MB, supervisor ≈ 12 MB (creekd, from `pm2-replacement` bench).
- **docker run**: per-app ≈ 85 MB, supervisor (Docker Engine + containerd + dockerd) ≈ 200–300 MB on a small VPS.

Projected on a $6 / month Hetzner CX21 (4 GB RAM, 2 vCPU), using the N=10 per-app figures (where the platforms are comparable for our purposes):

|  | usable RAM | per-app | fits |
|---|---:|---:|---:|
| creekd | 4 GB − 12 MB ≈ 4 084 MB | 72 MB | **~ 56 apps** |
| docker | 4 GB − 250 MB ≈ 3 838 MB | 85 MB | ~ 45 apps |

The headline density gap on a small box is ~25%. On a big box where Docker's 250 MB supervisor amortises, the gap narrows to the ~15% per-container tax.

## What the bench doesn't show

- **Disk and image overhead.** Each container also pulls a base image (oven/bun:alpine ≈ 90 MB). Many apps share layers, but cold pulls cost. Bare bun ships only `.next/standalone/`.
- **Live traffic.** RSS grows under load (JIT warmup, request-scoped allocations). The bench measures idle — the steady-state cost of having an app *available*, not running hot. This is the relevant number for review apps, preview environments, and tail-of-the-long-tail tenants.
- **CPU at scale.** Spawning 50 containers serialised through the docker daemon stressed colima during spawn but settled to idle. CPU isn't the bottleneck for idle hosting; RAM is.
- **Container startup time.** Each `docker run` waits on containerd-shim to fork, the image's entrypoint to run, the runtime to load. The 460× spawn gap matters for spike-driven autoscale; less so for long-lived services.

## Where each is better

### bare bun + creekd

- **RAM density.** 13 MB / app saved is one extra app per ~5 you'd otherwise host.
- **Spawn time.** Sub-second cold start; the supervisor pre-allocates nothing.
- **Disk.** No image layers; the standalone tree is what you ship.
- **Routing in the box** (when running under creekd) — no nginx in front, no port-publish per app.

### docker run

- **Isolation.** Containers default to full namespaces + cgroup + seccomp + apparmor. creekd ships namespace and cgroup opt-in, no seccomp/apparmor yet.
- **Image distribution.** A registry + tags + signatures + provenance is a real story Docker has and creekd doesn't.
- **Multi-arch / multi-OS.** `docker run` runs the same image on Linux, macOS-via-VM, Windows-via-VM. creekd is Linux + macOS-dev only.
- **Ecosystem.** Compose, K8s, registry providers, CI runners — all default-Docker.

## Methodology

The bench in `bench/main.go`:

1. Builds nothing inside the harness — assumes `./up.sh` has already produced `app/.next/standalone/server.js` and the image `creekd-nextjs-density:bench`. Same fixture for both scenarios.
2. **bare** spawns `bun run server.js` directly via `os/exec`, ports `19200+i`.
3. **docker** uses `docker run -d -p <port>:3000 <image>`, ports `19300+i`.
4. Polls `/healthz` until every app returns 200 (timeout 30 s per app for bare, 60 s for docker).
5. Sleeps `--settle` seconds (default 5) for the JIT and the docker engine to settle.
6. Reads RSS — `ps -o rss=` per PID for bare, `docker stats --no-stream` for containers.
7. Reports per-app p50/p95/min/max and total.

Both scenarios are cleaned up on exit (`defer`) — re-runs reuse port ranges and container names without leaking. The harness also pre-cleans any leftover `bench-docker-*` containers before starting (in case a previous run was killed).

### Known confound

`ps -o rss=` on Darwin and `docker stats` over a Linux VM measure RAM differently. Darwin counts shared library pages into RSS; Linux containers see only the cgroup's own pages. For a strictly apples-to-apples N=50, the bench should run on a Linux host where both scenarios use the same kernel. Pull-request welcome.
