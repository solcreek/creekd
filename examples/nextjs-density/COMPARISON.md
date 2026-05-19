# nextjs-density — bare bun vs `docker run`

Per-app RAM for an idle Next.js standalone, measured both ways on the same Linux host.

## Measured

### Linux (Hetzner cx33, x86_64, 4 vCPU / 8 GB RAM, Ubuntu 24.04)

```
Next.js 16.2.3, Bun 1.3.14, Docker 29.1.3
@solcreek/adapter-creekd 0.1.1
N=50 apps per scenario, 10s settle
```

| Metric | bare bun | docker run | docker tax |
|---|---:|---:|---:|
| per-app PSS p50 | **47.2 MB** | n/a | — |
| per-app PSS p95 | 48.9 MB | n/a | — |
| per-app RSS p50 (cgroup-scoped for docker) | 88.2 MB ¹ | **68.3 MB** | — |
| **honest per-app cost** (bare PSS vs docker RSS) | **47.2 MB** | **68.3 MB** | **+21.1 MB (1.45×)** |
| total PSS / total docker RSS (N=50) | **2.32 GB** | **3.34 GB** | **+1.02 GB (1.44×)** |
| MemAvailable delta (kernel truth, N=50) | −2.30 GB | −3.74 GB | **1.63×** |
| spawn-all wallclock | 561 ms | 25 407 ms | docker **45× slower** |
| all-healthy wallclock | 19 780 ms ² | 6 974 ms | — |

¹ `ps -o rss=` overcounts on Linux: each bare bun process maps ~30 MB of shared libraries (libbun.so, libc) and Linux RSS double-counts them. PSS (Proportional Set Size, `/proc/<pid>/smaps_rollup`) amortises those pages across the processes that share them — that's the cgroup-equivalent number on the bare side.

² Bare bun's all-healthy took longer than docker's because 50 Bun JIT warmups competed for 4 cores simultaneously; docker's sequential `docker run` already serialised the spawn, so the first containers were warm by the time the last one launched. Steady-state CPU was idle for both at sampling time.

Reproduce:

```bash
./up.sh
go run ./bench -n 50 -settle 10
```

The bench reports both raw RSS (the surface number that confuses many comparisons) and PSS (the honest one on Linux). On macOS where smaps_rollup isn't available, only RSS is reported — see "Why macOS numbers differ" below.

## What this means for density

> Given a fixed RAM budget, how many idle Next.js apps fit?

Using the honest per-app cost (bare PSS vs docker RSS), plus typical supervisor overhead:

- **bare bun / creekd**: per-app ≈ 47 MB, supervisor ≈ 12 MB (creekd, [pm2-replacement bench](../pm2-replacement/COMPARISON.md))
- **docker run**: per-app ≈ 68 MB, supervisor ≈ 200–300 MB (Docker Engine + containerd + dockerd combined)

Projected on a $6 / month Hetzner cx23 (4 GB RAM, 2 vCPU):

|  | usable RAM | per-app | fits |
|---|---:|---:|---:|
| creekd | 4 GB − 12 MB ≈ 4 084 MB | 47 MB | **~ 87 apps** |
| docker | 4 GB − 250 MB ≈ 3 838 MB | 68 MB | ~ 56 apps |

That's a **55% density advantage** on the constrained box. The MemAvailable-delta-based check (1.63×) gives roughly the same answer from a different angle.

### Why macOS numbers differ

We previously ran the same bench from `colima` (macOS host, Linux Docker VM) and saw a much smaller delta — 1.18× per-app at N=10, and an inverted ratio at N=50. Two reasons:

1. **Platform asymmetry.** Bare bun ran natively on Darwin; docker bun ran in the Linux VM. Different kernels' memory accounting can't be compared directly.
2. **RSS vs PSS confusion** (the bigger one). Darwin `ps -o rss=` and Linux RSS report different things. Neither amortises shared library pages the way PSS does. On macOS smaps_rollup isn't available, so the bench falls back to RSS for the bare side — that's why the macOS results overstate bare's cost.

The Linux PSS reading is the canonical density number. The macOS run still works as a sanity check that the bench harness behaves the same on both platforms; it just shouldn't be cited as the density story.

## Pushing density: zswap and the real ceiling

What happens when you push more apps onto the same host than naive PSS math says fits?

Same Hetzner cx33 host, with `zswap` enabled (`lz4` compressor, `zsmalloc` zpool, 50% max pool, 4 GB swapfile) and Transparent Huge Pages off. Bare bun only — docker daemon's cgroup accounting doesn't expose the same knob.

| N | per-app PSS | total PSS | MemAvail delta | zswap pool peak | disk writeback ¹ | verdict |
|---:|---:|---:|---:|---:|---:|---|
| 50 | 47 MB | 2.32 GB | -2.30 GB | 0 | 0 | comfortable |
| 100 | 48 MB | 4.68 GB | -4.69 GB | ~0 | ~0 | comfortable |
| **150** | **48 MB** | **6.88 GB** | **-6.89 GB** | 58 MB | **6 MB** (negligible) | **sweet spot, no tail risk** |
| 200 | 30 MB ² | 6.03 GB | -7.01 GB | **2.84 GB peak** | **333 MB** ❌ | fit, but tail latency |

¹ `written_back_pages` × 4 KB. Zswap writes pages to disk swap only when its in-RAM compressed pool fills up (capped at 50% of RAM here). Any non-zero number means some app's idle memory now lives on disk; the first request to that app pays a 10-100 ms swap-in.

² The N=200 per-app PSS dropping to 30 MB isn't Bun getting lighter — it's the kernel reclaiming idle pages out of the working set under pressure. The *physical cost* didn't drop; it just moved into zswap and disk swap. The honest number is the MemAvailable delta column, which kept climbing linearly.

### What this means in practice

- **N=150 is the real ceiling on an 8 GB host** for production traffic patterns. Per-app PSS stays at 48 MB; zswap engages lightly without falling through to disk; tail latency is unaffected.
- **N=200 is achievable but trades tail latency for density.** 333 MB of pages got pushed to disk swap. Those apps' first request after a quiet window costs an extra 10-100 ms. For review-app-style "wake on request" workloads that's fine; for prod traffic, it's not.
- **zswap is worth ~30% more apps before the tail-risk band**, not the 2-3× I'd guessed before measuring. Linux's reclaim is already aggressive at amortising idle pages — zswap mostly helps the few-percent of dirty/heap pages that survive reclaim.
- The same proportional ceiling on a 4 GB cx23: comfortable at ~75 idle apps, stretched at ~100. (8 GB box hit comfortable at 150; halve it for 4 GB, then subtract a bit more for OS baseline.)

Enabling zswap:

```bash
echo Y        > /sys/module/zswap/parameters/enabled
echo lz4      > /sys/module/zswap/parameters/compressor
echo zsmalloc > /sys/module/zswap/parameters/zpool
echo 50       > /sys/module/zswap/parameters/max_pool_percent
# plus a swap file for zswap to fall through to:
fallocate -l 4G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
```

Persist via `/etc/sysctl.d/` or systemd unit if you want it across reboot.

## What the bench doesn't show

- **Disk and image overhead.** Each container also pulls a base image (`oven/bun:alpine` ≈ 90 MB). Many apps share layers, but cold pulls cost. Bare bun ships only `.next/standalone/`.
- **Live traffic.** RSS / PSS grow under load (JIT warmup, request-scoped allocations). The bench measures idle — the steady-state cost of having an app *available*, not running hot. That's the relevant number for review apps, preview environments, and tail-of-the-long-tail tenants.
- **Container startup latency.** Each `docker run` waits on containerd-shim to fork, the image's entrypoint to run, the runtime to load. The 45× spawn gap matters for spike-driven autoscale; less so for long-lived services.
- **What runs on the same hardware besides Next.js.** Real density is constrained by traffic shape, disk I/O, and any persistent state, not just RAM.

## Where each is better

### bare bun + creekd

- **RAM density.** 21 MB / app saved is ~one extra app per ~3 you'd otherwise host.
- **Spawn time.** Sub-second cold start; the supervisor pre-allocates nothing.
- **Disk.** No image layers; the standalone tree is what you ship.
- **Routing in the box** (when running under creekd) — no nginx in front, no port-publish per app.

### docker run

- **Isolation.** Containers default to full namespaces + cgroup + seccomp + apparmor. creekd ships namespace and cgroup opt-in, no seccomp/apparmor yet.
- **Image distribution.** A registry + tags + signatures + provenance story Docker has and creekd doesn't.
- **Multi-arch / multi-OS.** `docker run` runs the same image on Linux, macOS-via-VM, Windows-via-VM. creekd is Linux + macOS-dev only.
- **Ecosystem.** Compose, K8s, registry providers, CI runners — all default-Docker.

## Methodology

The bench in `bench/main.go`:

1. Builds nothing inside the harness — assumes `./up.sh` has already produced `app/.next/standalone/server.js` and the image `creekd-nextjs-density:bench`. Same fixture for both scenarios.
2. **bare** spawns `bun run server.js` directly via `os/exec`, ports `19200+i`.
3. **docker** uses `docker run -d -p <port>:3000 <image>`, ports `19300+i`.
4. Polls `/healthz` until every app returns 200.
5. Sleeps `--settle` seconds (default 5) for the JIT and the docker engine to settle.
6. Reads three metrics:
   - **RSS** — `ps -o rss=` per bare PID, `docker stats --no-stream` per container.
   - **PSS** — `/proc/<pid>/smaps_rollup` per bare PID (Linux only). For docker we use the cgroup-scoped `docker stats` number, which is already proportional in spirit.
   - **MemAvailable delta** — `/proc/meminfo MemAvailable` before bench-start and after settle. Coarse (includes page cache) but kernel-honest.
7. Reports per-app p50/p95/min/max and the totals.

The harness pre-cleans any leftover `bench-docker-*` containers before starting (in case a previous run was killed) and `defer`-cleans both scenarios on exit.

### Why PSS

When 50 Next.js apps run as separate processes, they all map the same `libbun.so`, `libc.so`, etc. The kernel keeps ONE copy of those pages and lets every process reference it. `ps -o rss=` reports each process's total mapped memory, which means the same physical page is counted 50 times across the column.

PSS (`/proc/<pid>/smaps_rollup`) divides each shared page by the number of processes sharing it. Sum-of-PSS converges on actual physical RAM consumed by the group, which is what "how many apps fit" really asks.

Docker doesn't have this problem because each container's accounting is naturally cgroup-scoped: shared pages between containers (rare — different rootfs) aren't double-counted.
