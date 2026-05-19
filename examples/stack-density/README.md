# stack-density

How much RAM does one idle app cost, across different stacks? The answer changes the "how many fit per host" math by ~5× from the lightest to the heaviest.

This bench measures **per-app PSS** (Proportional Set Size, from `/proc/<pid>/smaps_rollup`) for four stacks at N=20 idle apps each:

- **bun-hello** — `Bun.serve()` with no framework, the floor
- **hono** — Hono on Bun, a minimal API framework
- **sveltekit** — SvelteKit with `@sveltejs/adapter-node`, run on Bun
- **astro** — Astro SSR with `@astrojs/node` (standalone), run on Bun

The Next.js number from [`nextjs-density/`](../nextjs-density/) sits at the top of the range — useful as the heaviest mainstream reference.

PSS is the honest "marginal RAM cost per app" — it amortises shared library pages across the processes that map them. Raw RSS (`ps -o rss=`) double-counts those, so 20 Bun processes each report ~40 MB RSS but real physical use is closer to 9 MB / app. See [`../nextjs-density/COMPARISON.md`](../nextjs-density/COMPARISON.md#methodology) for the PSS rationale in full.

## Run

```bash
# On a Linux host with bun + node + pnpm + npm available:
./bootstrap.sh   # bootstraps 4 fixtures into ./stacks/  (~3-5 min)
./measure.sh     # spawns N=20 per stack, samples PSS    (~1-2 min)

# Higher pressure:
N=50 SETTLE=15 ./measure.sh
```

`bootstrap.sh` is idempotent — re-running skips fixtures already built. To start clean: `rm -rf stacks/`.

## Why Linux-only

`/proc/<pid>/smaps_rollup` doesn't exist on macOS. The bench could fall back to `ps -o rss=` there, but the PSS-vs-RSS distinction is the whole point — RSS double-counts shared pages and turns the comparison into noise. On macOS, ssh into a small Linux VPS (Hetzner cx23 at $6/mo is sufficient) and run there.

## What the bench doesn't show

- **Live traffic.** Idle PSS is the cost of *having an app available*, not the cost of serving requests. Under load Bun processes grow to ~2-3× idle as JIT compiles request paths. The idle floor is the relevant number for review apps, preview environments, and long-tail tenants.
- **Cold start.** First request to a fresh app pays JIT warm-up (50-200 ms typical). The bench waits for `/healthz` 200 before sampling, so JIT for `/healthz` is warm; other routes haven't been touched.
- **Build artifact size on disk.** Different story — see each framework's own docs.

## See also

- [`../nextjs-density/`](../nextjs-density/) — same bench harness focused on Next.js + a `docker run` comparison
- [`STACKS.md`](STACKS.md) — measured numbers + analysis

## License

Apache 2.0, same as the rest of the repo.
