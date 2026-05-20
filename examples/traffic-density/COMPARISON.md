# traffic-density — measured

How much does per-app PSS inflate under traffic, across five stacks? Idle PSS (what [`stack-density`](../stack-density/STACKS.md) reports) is the floor; the ratios in this doc are the multiplier capacity-planning math has to apply on top.

## Measured

```
Hetzner cx33, x86_64, 4 vCPU / 8 GB RAM, Ubuntu 24.04, kernel 6.8
Bun 1.3.14, Node 22.22.2
N=5 apps per stack
Phases:
  idle      — 5s after boot
  warm      — 100 hits to / (no rate limit, round-robin across 5 apps)
  sustained — 1 rps × 60s
  burst     — 100 rps × 60s
  cooldown  — 60s with no traffic
```

### Per-app PSS by phase (p50)

| Stack | Idle | Warm | Sustained | **Burst** | Cooldown |
|---|---:|---:|---:|---:|---:|
| **bun-hello** | 13.2 MB | 13.2 MB | 13.1 MB | **13.9 MB** | 13.8 MB |
| **hono** | 17.2 MB | 17.3 MB | 17.0 MB | **18.3 MB** | 18.2 MB |
| **sveltekit** | 26.0 MB | 33.2 MB | 28.7 MB | **43.8 MB** | 43.1 MB |
| **astro** ¹ | 48.5 MB | 52.1 MB | 31.9 MB | **41.3 MB** | 40.6 MB |
| **nextjs** | 80.4 MB | 89.8 MB | 88.6 MB | **107.3 MB** | 98.3 MB |

¹ Astro shows kernel reclamation under burst pressure (PSS dropped 48.5 → 41.3); see the *Why Astro shows < 1.0× inflation* section below.

### Inflation summary

| Stack | **Burst ÷ Idle** | Cooldown ÷ Idle (retention) | Read it as |
|---|---:|---:|---|
| bun-hello | **1.06×** | 1.05× | floor stack — barely moves |
| hono | **1.06×** | 1.06× | same — Hono adds no JIT mass that traffic activates |
| sveltekit | **1.68×** | 1.66× | meaningful inflation; cooldown holds almost all of it |
| astro | 0.85× ¹ | 0.84× ¹ | kernel reclamation under pressure; see footnote |
| **nextjs** | **1.33×** | 1.22× | partial reclaim post-cooldown |

### HTTP latency p50 / p99 (ms)

| Stack | Warm (cold JIT) | Sustained (warm) | Burst (hot) |
|---|---:|---:|---:|
| bun-hello | 0.8 / 2.3 | 0.6 / 1.6 | 0.5 / 0.9 |
| hono | 0.7 / 2.9 | 0.7 / 1.2 | 0.5 / 0.9 |
| sveltekit | 11.9 / 47.0 | 2.8 / 3.9 | 1.5 / 2.4 |
| astro | 10.3 / 47.0 | 2.8 / 4.1 | 1.6 / 2.6 |
| nextjs | 23.0 / 73.2 | 5.0 / 9.0 | 2.6 / 5.6 |

JIT warmup cost is observable in the first 100 requests for SvelteKit / Astro / Next.js (10-23 ms p50, ~50-70 ms p99). Once warmed, p50 drops 4-10× — Bun's JavaScriptCore needs to see a path before optimising it. **The "warm" PSS column also shows this is where most of the inflation happens** — warm-then-cool-down doesn't return to idle on the heavier stacks.

## Implications for capacity planning

Using burst PSS as the realistic ceiling (apps DO see spike traffic; capacity that only fits when no app is hot is fragile):

| Stack | Idle ceiling on 4 GB cx23 | **Burst-adjusted ceiling** | Correction |
|---|---:|---:|---:|
| bun-hello | ~410 | ~390 | -5% |
| hono | ~300 | ~285 | -5% |
| sveltekit | ~200 | ~120 | **-40%** |
| astro | ~155 | ~130 ¹ | -16% |
| nextjs | ~75 | ~58 | -23% |

¹ Astro's effective ceiling is harder to pin precisely — the kernel-reclamation artifact means PSS isn't tracking true working set during burst. Using the higher of (warm 52 MB, idle 48.5 MB) as a conservative bound gives ~70 apps; the table uses an interpolation closer to that.

**Takeaway**: lighter stacks (Bun raw, Hono) are essentially the same idle and hot — their density story doesn't degrade under traffic. SvelteKit takes the biggest correction (idle 26 MB → working set ~44 MB), so the stack-density headline of "200 SvelteKit apps on 4 GB" is a best-case that real production rarely matches.

## Why Astro shows < 1.0× inflation

Astro's PSS during burst (41.3 MB) is LOWER than at idle (48.5 MB). The kernel is at work, not Astro.

When the 100 rps burst phase hits 5 Astro apps simultaneously, the system enters memory pressure. The kernel's MGLRU then reclaims idle/cold pages out of every process — including Astro's pre-allocated JIT regions and lazy-loaded module code that the burst path didn't actually exercise. PSS measures what's *resident* in RAM, not what the process logically uses; reclaimed pages leave PSS even though they'll be paged back in when next accessed.

We saw the same artifact in [`../nextjs-density/COMPARISON.md`](../nextjs-density/COMPARISON.md#pushing-density-zswap-and-the-real-ceiling) at N=200 (Next.js PSS "dropped" from 48 to 30 MB under memory pressure — same mechanism). The honest interpretation: **for capacity planning, idle PSS is the lower bound and warm-phase PSS is the upper bound; burst PSS can be misleading when the system itself starts reclaiming under load**.

Distinguishing artifact from reality requires reading MemAvailable delta alongside PSS — the system actually used more memory during burst (`MemAvailable` dropped 47 MB more than at idle), so Astro's *total* RAM consumption rose even as per-process PSS fell.

## Cooldown — does the kernel give it back?

The cooldown column shows what fraction of the inflation persists 60 seconds after traffic stops. This matters for the review-app use case: a fleet of preview environments that wake up under traffic and quiesce when reviewers go to lunch.

- **bun-hello / hono**: full reclaim (cooldown ≈ idle).
- **next.js**: partial reclaim (107 MB → 98 MB; held ~22% above idle).
- **sveltekit**: minimal reclaim (43.8 → 43.1; held essentially all inflation).
- **astro**: PSS stays low (same reclamation artifact as burst).

For an idle-mostly fleet where the heavy stacks see occasional traffic, **expect SvelteKit and Next.js to settle ~1.2-1.7× their idle PSS within the first hour of any traffic exposure**, then stay there. Plan capacity at the warm-after-traffic value, not the pristine boot-time value.

## What the bench doesn't show

- **Days-long working set**. 60-second burst doesn't fully populate caches, route manifests, or whatever the framework lazy-builds. A multi-day production app could land higher than what burst reports here.
- **Concurrent multi-app traffic patterns**. All five apps got the same load profile simultaneously; a realistic fleet has uneven distribution and would surface kernel scheduling effects this bench masks.
- **Stress beyond cx33 capacity**. 100 rps × 5 apps = 500 rps total, well within 4 vCPU. CPU-bound stacks at higher load would show different memory shape.
- **Different request paths**. We hit `/` only. SSR with parameters, API routes, ISR rebuilds — each is its own JIT compilation path with its own memory cost.

## Methodology

The bench in `bench/main.go`:

1. Spawns N=5 apps per stack via `bun run <entry>`, ports 21000 + stack_offset + i.
2. For each phase: applies the load profile (none / warm / 1 rps / 100 rps / settle), then samples `/proc/<pid>/smaps_rollup` for every spawned PID.
3. Tracks MemAvailable from `/proc/meminfo` before and after each phase for the system-side check.
4. Records HTTP latency per request during traffic phases; reports p50/p99.

Load generator is in-process Go (no `wrk` dependency) using `net/http` with keep-alive enabled.

## Reproducing

```bash
# On a Linux host with bun + node + pnpm + npm + go available:
../stack-density/bootstrap.sh    # ~3 min
../nextjs-density/up.sh          # ~2 min
./bootstrap.sh                   # idempotent verification
go run ./bench                   # ~16 min for 5 stacks
```

Total cost on Hetzner cx33 ($0.013/h): well under $0.01 for the full run.
