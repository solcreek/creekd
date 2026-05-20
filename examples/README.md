# Examples

Eight runnable recipes that each prove one slice of what creekd is for. Every example builds creekd + creekctl from source, boots the daemon locally, spawns one or more apps via `creekctl up`, and shows the result through curl. All self-contained — no shared state between them.

| Example | What it proves | Compares to | Linux required? |
|---|---|---|---|
| [`pm2-replacement/`](pm2-replacement/) | Multi-app supervision + HTTP routing + crash-recovery + memory caps on one host | **pm2** — measured 8.4× faster spawn, 4.9× leaner RSS, 300× faster memory-cap reaction ([COMPARISON.md](pm2-replacement/COMPARISON.md)) | no (degrades on macOS) |
| [`sandboxed-eval/`](sandboxed-eval/) | Per-spawn Linux sandbox (chroot + PID/mount/UTS namespaces + hard memory cap + kernel OOM) for cooperative-but-buggy workloads | **docker run** — measured 2.6× faster cold spawn for matched isolation ([COMPARISON.md](sandboxed-eval/COMPARISON.md)) | yes |
| [`review-apps/`](review-apps/) | Side-by-side preview environments + zero-downtime blue-green redeploy on one host | Heroku Review Apps / Vercel Preview Deployments shape, without their build pipeline | no (degrades on macOS) |
| [`bun-app/`](bun-app/) | `--runtime bun --entry server.ts` end-to-end: Bun.serve, bun:sqlite, SSE streaming through the dispatch reverse proxy | n/a — this is the runtime-coverage demo | no (needs Bun installed) |
| [`nextjs-density/`](nextjs-density/) | Idle RAM density: how many Next.js apps fit on one host with `@solcreek/adapter-creekd` | **`docker run`** — measured 1.45× per-app PSS overhead, 1.63× total kernel RAM, 45× faster spawn on Linux ([COMPARISON.md](nextjs-density/COMPARISON.md)) | no (uses docker for the comparison side; bare side runs anywhere) |
| [`stack-density/`](stack-density/) | Per-app idle PSS across stacks: Bun (8.8 MB) / Hono (12 MB) / SvelteKit (18 MB) / Astro (23 MB) / Next.js (48 MB) | Across stacks rather than supervisors — picking a lighter framework dwarfs every other density lever ([STACKS.md](stack-density/STACKS.md)) | yes (needs /proc/&lt;pid&gt;/smaps_rollup) |
| [`traffic-density/`](traffic-density/) | Per-app PSS through idle → warm → sustained → burst → cooldown across all 5 stacks | Layered on top of stack-density to measure the **traffic inflation multiplier** capacity-planning math depends on — bun-hello / hono ~1.06×, sveltekit ~1.68×, next.js ~1.33× ([COMPARISON.md](traffic-density/COMPARISON.md)) | yes |
| [`cgroup-memory-tuning/`](cgroup-memory-tuning/) | What's the right `memory.high` default? False-positive sweep + containment + sibling-impact across three phases | Empirical justification for `CREEKD_DEFAULT_MEMORY_HIGH=256M`: 0 throttle events at idle, 11% overshoot under runaway, +2 ms p50 to neighbors ([RESULTS.md](cgroup-memory-tuning/RESULTS.md)) | yes (cgroup v2 + root) |

## How to run any one of them

```bash
cd examples/<name>
./up.sh            # builds creekd + creekctl + the example app, boots, spawns
# ...poke around...
./down.sh          # tears it all down
```

Each example's `up.sh` is self-contained — separate `bin/`, separate `state/`. Only one example runs at a time because creekd's dispatch listener always binds `127.0.0.1:9000`; `down.sh` is fast, so switching between examples is just `down.sh && cd ../other && ./up.sh`.

## Which one to read first

- **Coming from pm2 / supervisord / runit**: [pm2-replacement](pm2-replacement/) has the head-to-head numbers and the migration pitch.
- **Running AI tool calls / user-submitted code / CTF judges**: [sandboxed-eval](sandboxed-eval/) shows the per-spawn isolation pattern, with the honest "where docker still wins" section.
- **CI / preview environments**: [review-apps](review-apps/) walks the deploy workflow and points at the Caddy-front-door wiring needed to lift it to production.
- **Already on Bun and curious if creekd hosts it**: [bun-app](bun-app/) shows the `--runtime bun` path with native Bun features.

## Benchmarks

Two of the examples (`pm2-replacement` and `sandboxed-eval`) carry runnable bench tools that produce head-to-head numbers on the same machine. They are *not* run in CI — runner CPU is too noisy to gate on — but the methodology is documented in each example's `COMPARISON.md` and reproducing on your own hardware is one command:

```bash
cd examples/pm2-replacement && ./up.sh && go run ./bench -n 20
cd examples/sandboxed-eval  && ./bench/run.sh
cd examples/nextjs-density  && ./up.sh && go run ./bench -n 10
cd examples/stack-density   && ./bootstrap.sh && ./measure.sh   # Linux-only
cd examples/traffic-density && ./bootstrap.sh && go run ./bench  # Linux-only, ~16 min
cd examples/cgroup-memory-tuning && sudo ./run.sh                 # Linux-only, ~5 min
```

The numbers in the comparison docs were measured on a darwin/amd64 host with Docker Desktop. Your absolute numbers will differ; the ratios should be in the same ballpark.
