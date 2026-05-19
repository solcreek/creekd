# Stack density — measured numbers

Per-app idle RAM (PSS, KB) for 4 stacks on the same Linux host. All ran under Bun 1.3.14, with N=20 instances each.

## Measured

```
Hetzner cx33, x86_64, 4 vCPU / 8 GB RAM, Ubuntu 24.04, kernel 6.8
Bun 1.3.14, Node 22.22.2
N=20 apps per stack, 10s settle
```

| Stack | PSS p50 | PSS p95 | RSS p50 ¹ | Total PSS (N=20) |
|---|---:|---:|---:|---:|
| **bun-hello** | **8.8 MB** | 8.9 MB | 37.5 MB | 178 MB |
| **hono** | **12.0 MB** | 12.5 MB | 43.2 MB | 237 MB |
| **sveltekit** | **18.4 MB** | 22.1 MB | 56.5 MB | 373 MB |
| **astro** | **22.7 MB** | 23.0 MB | 63.5 MB | 455 MB |
| **next.js 16** (from [nextjs-density](../nextjs-density/COMPARISON.md)) | **47.6 MB** | 47.9 MB | 65.9 MB | 952 MB ² |

¹ RSS systematically overstates the marginal cost — 20 Bun processes each map the same `libbun.so` (~25-30 MB worth of code), and Linux RSS counts every mapping in every process. PSS divides each shared page by the number of mappers, so sum-of-PSS converges on actual physical RAM used by the group.

² Next.js row scales the per-app PSS to N=20 for the column to be comparable; the [original bench](../nextjs-density/COMPARISON.md#measured) measured at N=50/100/150.

## Per-stack framework overhead

What sits on top of the Bun process baseline:

| Stack | PSS over `bun-hello` | What it adds |
|---|---:|---|
| bun-hello | — | (floor: JS runtime + one process) |
| hono | +3.2 MB | router, response helpers |
| sveltekit | +9.6 MB | framework runtime + SSR machinery + Vite's transformed graph |
| astro | +13.9 MB | island runtime + asset pipeline + content collections |
| next.js 16 | +38.8 MB | React Server Components runtime, ISR cache, middleware infra, App Router manifest, image optimization, edge polyfills |

The Next.js delta (~39 MB / app over the floor) is **4× the SvelteKit delta** and **12× the Hono delta**. The capability surface justifies it — Server Components, granular caching, image optimization, middleware — but the cost is real.

## How many idle apps fit per host

Linear extrapolation from per-app PSS, allowing 400 MB for OS + supervisor baseline. These are *idle* ceilings — live traffic shifts the number down ~2-3× as JIT warms up.

| Host | Usable | bun-hello | hono | sveltekit | astro | next.js |
|---|---:|---:|---:|---:|---:|---:|
| 4 GB (e.g. cx23) | 3.6 GB | ~410 | ~300 | ~200 | ~155 | ~75 |
| 8 GB (e.g. cx33) | 7.6 GB | ~860 | ~640 | ~420 | ~340 | ~155 |
| 16 GB | 15.6 GB | ~1750 | ~1300 | ~860 | ~700 | ~330 |

For comparison: 50 idle Bun processes consumed ~2.3 GB PSS (`nextjs-density` measured this at N=50/100/150 with the same PSS metric — see that doc for the saturation curve and where zswap starts to matter).

## What the numbers mean for stack choice

- **Picking a lighter SSR framework is the single biggest density lever**, dwarfing distro tuning, kernel parameters, or even zswap. A SvelteKit fleet fits ~2.6× more apps per host than the same code on Next.js.
- **Bun specifically helps** because the runtime is small (~9 MB / process floor). The same SvelteKit fixture under Node would be measurably heavier (Node baseline ≈ 3× Bun, [bun benchmark](https://bun.sh/docs/runtime/performance)).
- **Framework overhead amortises poorly across apps** — 50 SvelteKit apps don't share their SvelteKit copy in a useful way (each is its own module graph; PSS only catches the shared C libraries underneath).

## Methodology

See [`README.md`](README.md) for the steps. The measurement script is short and direct: spawn N processes via `bash &`, wait for `/healthz` on every port, sleep `SETTLE`, then read PSS from `/proc/<pid>/smaps_rollup`. No Docker, no supervisor, no creekd — direct process-to-kernel measurement.

PSS is the right metric here because:
- **`ps -o rss=`** counts every mapped page in every process (overcounts shared by N for N processes mapping the same library).
- **`docker stats`** cgroup-scoped memory is already proportional in spirit — that's why the docker side of [nextjs-density](../nextjs-density/COMPARISON.md) didn't need a separate correction.
- **PSS** divides shared pages by the number of mappers, so sum-of-PSS converges on actual physical RAM consumed by the group.

The PSS numbers above are the same answer the kernel reaches when it decides whether to OOM-kill — that's the canonical density number.

## Reproducing

```bash
# Provision any 4-8 GB Linux box with bun + node + pnpm + npm.
# Then in this directory:
./bootstrap.sh
./measure.sh
```

Output to stdout; no log files, no state. Re-run as many times as you want — same numbers within ~5%.

A Hetzner cx23 ($6/month, billed hourly) is plenty for this bench. The whole run takes ~10 minutes including bootstrap.
