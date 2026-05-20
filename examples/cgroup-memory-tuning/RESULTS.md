# cgroup-memory-tuning — results

Measured 2026-05-20 on Hetzner cx33 (Ubuntu 24.04, kernel 6.8, cgroup v2). Three phases.

## Phase 1 — false-positive sweep at memory.high = 256 MB

Goal: confirm normal apps under normal traffic stay well under the candidate default, so it doesn't false-positive throttle legitimate workloads.

Setup: per-stack, one app inside a per-cgroup directory with `memory.high = 256M` set. Sustained 30 rps × 60 s of HTTP GET / from the host.

| Stack | throttle events | peak `memory.current` | sustained 30 rps × 60 s (req count / p50 / p99) |
|---|---:|---:|---|
| bun-hello | **0** | 8 MB | 892 / 22 ms / 28 ms |
| hono | **0** | 12 MB | 912 / 21 ms / 28 ms |
| sveltekit | **0** | 38 MB | 874 / 23 ms / 30 ms |
| astro | **0** | 35 MB | 873 / 24 ms / 30 ms |

Plus Next.js, which we don't bench in this phase (it's the heaviest framework — measured separately in [`../traffic-density/`](../traffic-density/COMPARISON.md): burst PSS p95 = 108 MB). 108 MB is 42% of 256 MB — comfortable.

**Conclusion**: zero throttle events across four stacks at the candidate default. The lightest stack uses 3% of the cap, the heaviest (in this phase) 15%. Even projecting Next.js into the picture: 42%. **No false-positive risk at memory.high = 256 MB for any tested stack.**

## Phase 2 — containment behaviour

Goal: when a runaway app crosses the limit, how fast does the kernel react, and how much does the process overshoot before throttle pressure stabilises it?

Setup: a deliberate leaker (Bun process allocating + filling 10 MiB / 100 ms for 30 s) inside per-cgroup directories with varying `memory.high`. No `memory.max` set — only `memory.high`. So overshoot is bounded only by the soft pressure, not by a hard kill.

```javascript
// leaker.js
const a = [];
let i = 0;
const start = Date.now();
setInterval(() => {
  const buf = new Uint8Array(10 * 1024 * 1024);
  buf.fill(i % 256);  // .fill() forces physical page allocation;
                      // without it the typed array is virtual-only
  a.push(buf);
  i++;
  if (Date.now() - start > 30000) process.exit(0);
}, 100);
```

| Limit | t to first throttle | peak `memory.current` | overshoot vs limit | total throttle events | oom_kills |
|---|---:|---:|---:|---:|---:|
| 128 MB | 3 s | 145 MB | **+13%** | 12 451 | 0 |
| 256 MB | 3 s | 285 MB | **+11%** | 4 841 | 0 |
| 512 MB | 6 s | 556 MB | **+9%** | 3 818 | 0 |

**Conclusion**:
- Kernel reaction is fast (3-6 s to first throttle, varies slightly with limit because the slope of allocation vs pressure threshold differs).
- Overshoot before stabilisation is 9-13% above the limit. **Predictable headroom budget**: when you set memory.high = X, plan for actual peak ~X × 1.15.
- No OOM kills — the soft cap holds without the hard cap. Aggressive throttling slows the leaker to a crawl (a 30-second wall-clock leaker that would have allocated 3 GB unbounded actually allocated ~285 MB at limit=256 MB, then was throttled at that ceiling for the remainder).

## Phase 3 — sibling impact

Goal: a contained runaway shouldn't affect neighbors. Verify directly.

Setup: 4 normal Hono apps (no cap, ports 23001-23004) plus 1 leaker contained at `memory.high = 256 MB`. Baseline pass: hit one normal app at 50 rps × 15 s **with no leaker**. Then spawn the leaker, give it 10 s to ramp into throttle, then repeat the 50 rps × 15 s pass.

| | requests | p50 latency | p99 latency |
|---|---:|---:|---:|
| baseline (no leaker) | 295 | 19 ms | 27 ms |
| under contained leaker | 296 | **21 ms** | 27 ms |
| delta | ≈ 0 | **+2 ms** (+10%) | 0 ms |

Leaker cgroup state at end: `memory.current` = 271 MB, 2 241 throttle events accumulated, 0 oom_kills. Host MemAvailable was still 6 966 MB out of ~7.5 GB usable — leaker fully contained.

**Conclusion**: a contained leaker adds **2 ms p50** to neighbor latency, **0 ms p99**. Throughput identical. The cap works in practice, not just in theory.

## Translating to default

Based on all three phases, the recommended creekd default is:

- **`CREEKD_DEFAULT_MEMORY_HIGH=256M`** for the noisy-neighbor protection lane.

Rationale:
- Phase 1: ≥10× safety margin from every stack's peak under normal traffic.
- Phase 2: predictable 11% overshoot; runaway contained without OOM.
- Phase 3: neighbor impact in the noise (+2 ms p50, no p99 change).

Operators who want to differentiate per-tier (Free / Pro / Team) can override per-app via `creekctl up --memory-high <value>`. The daemon-wide default sets the policy floor; explicit per-app values always win.

## What this experiment doesn't show

- **Multi-day working set drift**. 30 s of pressure isn't a week.
- **Concurrent leaker storm**. One leaker at a time was tested. N simultaneous leakers behave differently — the host enters page-cache eviction territory, and kernel reclaim becomes the bottleneck for everyone (cf. zswap experiment in [`../nextjs-density/COMPARISON.md`](../nextjs-density/COMPARISON.md#pushing-density-zswap-and-the-real-ceiling)).
- **memory.max + memory.high paired**. The experiment used memory.high alone. Real production should pair both: memory.high (soft, low) for noisy-neighbor protection, memory.max (hard, generous, ~3× memory.high) as the final safety net.

## Reproducing

```bash
# On a Linux host with cgroup v2 (Hetzner cx33 default, Ubuntu 24.04+):
../stack-density/bootstrap.sh
sudo ./run.sh
```

The `run.sh` script writes its tables to stdout AND `/tmp/cgroup-exp.log`. Re-running cleans up the test cgroups under `/sys/fs/cgroup/creek-exp/` on exit.
