# nextjs-density

How many idle Next.js apps fit on a box? Compares two ways to host the same Next.js standalone build:

- **bare bun** — `bun run server.js` per app. This is the lower bound: every supervisor that just *runs the process* (creekd, systemd, runit, pm2 with `--interpreter bun`) sees this number, because nothing is added on top.
- **docker run** — `docker run -d` of an image around the same standalone. Each container brings its own `containerd-shim` plus the engine's per-container accounting.

The fixture is in `app/`: one page, one `/healthz`, the `@solcreek/adapter-creekd` adapter wired into `next.config.ts`.

## Run

```bash
./up.sh                          # builds the fixture + docker image
go run ./bench -n 10             # both scenarios, 10 apps each
go run ./bench -n 50 -settle 10  # 50 apps each — needs a host with 8 GB free
./down.sh                        # cleanup
```

The bench:

1. spawns N apps (bare or in containers) on consecutive ports;
2. waits for `/healthz` to return 200 on each;
3. sleeps `--settle` seconds for steady state;
4. samples RSS — `ps -o rss=` for bare, `docker stats --no-stream` for containers;
5. reports per-app p50 / p95 / min / max and the total.

## Why this matters

The headline "modern PaaS density" claim — *Creek hosts N tenants on a small VPS for $X / month* — rests entirely on the per-app overhead being small. If every app dragged a Docker-sized container along, the math wouldn't work. The bench measures that overhead directly.

Numbers and trade-off discussion: [COMPARISON.md](COMPARISON.md).

## Adapter version

This example pins `@solcreek/adapter-creekd` from npm. The adapter is the user-facing surface; the manifest it emits (`.creek-creekd/manifest.json`) is what `creekctl up --from <manifest>` consumes on first deploy and `creekctl deploy --from <manifest>` consumes on every subsequent rebuild — same precedence rule for both (CLI flags override the manifest's runtime / entry / port). If you're hacking on the adapter locally, swap the dep for a `file:../../../../adapter-creekd` link.
