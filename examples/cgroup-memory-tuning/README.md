# cgroup-memory-tuning

What's the right `memory.high` + `memory.max` default pair for a multi-tenant fleet? This example documents the experiment that informed creekd's chosen defaults and provides the reproducer scripts.

cgroup v2 memory caps come in two flavors:

- **`memory.high`** — the **soft** cap. Cross it and the kernel throttles allocations + aggressively reclaims pages, but does NOT OOM-kill. Right tool for "one app spiking shouldn't take down its neighbors" — degrades the offender, leaves siblings untouched.
- **`memory.max`** — the **hard** cap. Cross it and the kernel OOM-kills inside the cgroup. The safety net for the rare case where memory.high can't keep up.

This example answers six questions across two paired experiments:

**memory.high experiment** (`run.sh`):
1. **False-positive rate** at the candidate default — do normal apps under normal traffic ever cross 256 MB and get throttled?
2. **Containment** — when a runaway crosses the soft cap, how fast does the kernel react and how much overshoot is there?
3. **Sibling impact** — does a contained runaway affect neighbor latency?

**memory.max experiment** (`run_max.sh`):

4. **Paired containment** — with memory.high already enforced, does memory.max ever fire under a steady leaker?
5. **Defeat memory.high alone** — can any allocation pattern cross the soft cap faster than throttle can react?
6. **Multi-leaker storm** — 4 simultaneous contained leakers, does the host stay healthy?

## Findings

See [`RESULTS.md`](RESULTS.md) for the measured tables. Short version:

**memory.high default = 256M**:
- Safe for every stack measured. Per-app peak under sustained 30 rps × 60 s: bun-hello 8 MB, hono 12 MB, sveltekit 38 MB, astro 35 MB. Even Next.js (heaviest, 107 MB burst from [`../traffic-density/`](../traffic-density/COMPARISON.md)) uses ~42% — comfortable safety margin.
- Kernel reaction is fast (3-6 s to first throttle), overshoot small (109-113% of limit), sibling impact negligible (+2 ms p50, p99 unchanged).

**memory.max default = 1G**:
- memory.max **never fires** under steady leaker at any cap value tested (384M / 512M / 768M / 1G).
- No adversarial allocation pattern we constructed (single 768 MiB shot; 1 GiB/s sustained) defeats memory.high — all converge to the same ~278 MB peak.
- 4-leaker storm: 0 oom_kills, host MemAvailable −1108 MB / +6216 MB remaining. Containment scales linearly.
- 1G = 3.6× the empirical worst-case peak, comfortable headroom without false-positives.

**Non-decision finding** (worth flagging for monitoring design): under sustained memory.high throttle, the bun event loop frequently freezes — the process stays alive but stops responding. This is correct *containment* (no OOM, no neighbor impact) but means **operator dashboards need to detect unresponsiveness, not just OOM**. Belongs in supervisor health-check, not the cap value.

## Run

```bash
# On a Linux host (cgroup v2 required) with bun + node + pnpm + npm:
../stack-density/bootstrap.sh    # one-time fixture build for run.sh
sudo ./run.sh                    # Phases 1-3 (memory.high), ~5 min
sudo ./run_max.sh                # Phases 4-6 (memory.max), ~7 min
```

Each script writes a table per phase to stdout + a log under `/tmp/`. Cleanup is automatic on exit.

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
