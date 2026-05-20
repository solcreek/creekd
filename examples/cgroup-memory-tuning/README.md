# cgroup-memory-tuning

What's the right `memory.high` default for a multi-tenant fleet? This example documents the experiment that informed creekd's chosen default and provides the reproducer script.

`memory.high` is the cgroup v2 **soft** memory cap: cross it and the kernel throttles allocations + aggressively reclaims pages, but does NOT OOM-kill the cgroup. It's the right tool for "one app spiking shouldn't take down its neighbors" — degrades the offender, leaves siblings untouched. Distinct from `memory.max`, the hard cap that triggers the OOM killer.

This example answers three questions:

1. **False-positive rate at the candidate default**: do normal apps under normal traffic ever cross 256 MB and get throttled? (If yes, default is too aggressive.)
2. **Containment**: when a runaway app crosses the limit, how fast does the kernel react and how much overshoot is there before pressure stabilises it?
3. **Sibling impact**: does a contained runaway affect the latency of its neighbors?

## Findings

See [`RESULTS.md`](RESULTS.md) for the measured tables. Short version:

- **256 MB is a safe default for every stack** we measured. Per-app peak under sustained 30 rps × 60 s: bun-hello 8 MB, hono 12 MB, sveltekit 38 MB, astro 35 MB. Even the heaviest mainstream framework (Next.js 16, 107 MB burst from [`../traffic-density/`](../traffic-density/COMPARISON.md)) uses ~42% of the cap — comfortable safety margin.
- **Kernel reaction is fast**: first throttle event arrives 3-6 s after a leaker crosses the threshold.
- **Overshoot is small**: 109-113% of the limit before pressure stabilises (10 MB / 100 ms leaker case).
- **Sibling impact is negligible**: p50 latency +2 ms (10%), p99 unchanged. Throughput identical.

## Run

```bash
# On a Linux host (cgroup v2 required) with bun + node + pnpm + npm:
../stack-density/bootstrap.sh    # one-time fixture build
sudo ./run.sh                    # needs root for cgroup writes
```

`run.sh` is in three phases — false-positive sweep, containment, sibling impact — output table per phase. Full run ~5 minutes.

## Why memory.high specifically (not just memory.max)

| | What happens when limit is crossed |
|---|---|
| `memory.max` | Kernel OOM killer fires. Process dies. Brutal but final. |
| `memory.high` | Kernel throttles allocations + aggressive page reclaim. Process slows dramatically but stays alive. |

For multi-tenant defense in depth, you want **both**:

- `memory.high` low (e.g. 256 MB) → first line of pressure; spiking apps degrade in isolation.
- `memory.max` generous (e.g. 1 GB) → second line; runaway leaks eventually trigger cgroup-scoped OOM kill (not host-wide).

The example focuses on memory.high because that's the layer that lets a runaway *not become a sibling-killing event*. memory.max is straightforward and the kernel docs cover it well.

## Linux-only

cgroup v2 + writable `cgroup.subtree_control` required. Hetzner cx33 (cgroup v2 default, Ubuntu 24.04) is the reference host.

## See also

- [`../traffic-density/COMPARISON.md`](../traffic-density/COMPARISON.md) — per-app PSS under traffic, the data that lets us size the default. Heaviest measured stack (Next.js) lands at 107 MB burst; 256 MB cap is 2.4× that.
- [`../stack-density/STACKS.md`](../stack-density/STACKS.md) — idle PSS floor across the same five stacks.
