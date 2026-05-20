# traffic-density

Per-app PSS through five phases of traffic — idle, warm, sustained low, burst, cooldown — across the five stacks already bootstrapped by the [`stack-density`](../stack-density/) and [`nextjs-density`](../nextjs-density/) examples.

[`stack-density`](../stack-density/) reports the idle floor: 8.8 MB Bun raw, 12.0 MB Hono, 18.4 MB SvelteKit, 22.7 MB Astro, 47.6 MB Next.js. **This example measures the multiplier under traffic** — how much real-world load inflates that floor, and how much the kernel reclaims after traffic stops.

The inflation ratio (idle → burst) is the headline number. Capacity-planning math built on idle PSS alone undercounts production density; the burst number sets the safe ceiling.

## Run

```bash
# On a Linux host with bun + node + pnpm + npm + wrk available:
../stack-density/bootstrap.sh   # one-time fixture build
../nextjs-density/up.sh         # one-time Next.js standalone build
./bootstrap.sh                  # idempotent check
go run ./bench                  # default: N=5 apps per stack, ~10 min total

# Tweak:
N=10 go run ./bench
STACK=hono go run ./bench       # one stack at a time
```

## Phases

| Phase | What it does | What it tells us |
|---|---|---|
| **idle** | apps boot, sit for 5 s, sample PSS | floor — matches stack-density |
| **warm** | 100 hits to `/` per stack (parallel, max rate) | JIT-warmup cost — paths that exist but haven't been compiled |
| **sustained** | 1 rps × 60 s, round-robin across N apps | typical long-tail tenant working set |
| **burst** | 100 rps × 60 s | spike / hot endpoint — sets the realistic upper bound for capacity math |
| **cooldown** | wait 60 s with no traffic, re-sample | how much the kernel reclaims; critical for review-app density story |

## Why Linux-only

`/proc/<pid>/smaps_rollup` is Linux-only. macOS RSS overcounts shared library pages (see [`../nextjs-density/COMPARISON.md`](../nextjs-density/COMPARISON.md#methodology)). On macOS, ssh into a small Linux VPS (Hetzner cx33 at $10/mo, billed hourly — full run costs ~$0.05).

## Results

Numbers live in [`COMPARISON.md`](COMPARISON.md), updated after each clean Linux run.

## See also

- [`../stack-density/STACKS.md`](../stack-density/STACKS.md) — the idle floor numbers this bench layers traffic on top of.
- [`../nextjs-density/COMPARISON.md`](../nextjs-density/COMPARISON.md) — PSS methodology rationale + zswap engagement curve.
