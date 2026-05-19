# pm2-replacement

Three independent apps on one machine. Hostname-style routing from a single dispatch port. Auto-restart on crash. Real memory caps. One daemon, one CLI, one config-less invocation per app — the pieces of `pm2 + nginx + ulimit` that you'd otherwise wire by hand.

## What it shows

- `creekctl up` spawns three distinct app processes (`api`, `worker`, `cron`).
- Dispatch listener on `:9000` routes incoming HTTP to the right one via `X-Creek-App`.
- Each app has an independent memory cap and pid cap enforced by cgroup v2.
- Killing an app (or hitting `/crash`) triggers supervised auto-restart with a fresh PID; the dispatch route stays valid the whole time.

## Run it

```bash
./up.sh
```

This builds `creekd`, `creekctl`, and a tiny Go toy server, starts the daemon in the background, and spawns the three apps.

```bash
curl -H 'X-Creek-App: api'    http://127.0.0.1:9000/
# hello from api (pid=12345, host=127.0.0.1:9000)

curl -H 'X-Creek-App: worker' http://127.0.0.1:9000/
# hello from worker (pid=12346, host=127.0.0.1:9000)
```

### See them

```bash
./bin/creekctl ps
# ID       STATUS   PID      PORT   UPTIME      RESTARTS
# api      running  12345    18001  3s          0
# worker   running  12346    18002  3s          0
# cron     running  12347    18003  3s          0

./bin/creekctl stats api
# memory_used   1.2M / 64.0M
# pids_current  4
```

### Crash and recover

```bash
curl -H 'X-Creek-App: api' http://127.0.0.1:9000/crash
# crashing

# wait a beat, then:
./bin/creekctl get api
# ... restart_count: 1, new PID
```

The supervisor watched the child exit non-zero, applied the backoff policy, and re-spawned with the same config. No external watchdog, no entry in your crontab, no `pm2 startup`.

### Tear down

```bash
./down.sh
```

## What's not in this example

- TLS. Put Caddy / Cloudflare in front of `:9000`.
- Multi-host. Run one `creekd` per host with a load balancer in front (or use Cloudflare DNS round-robin for tiny scale).
- Web dashboard. CLI + JSON API only — `creekctl ps --json | jq` covers most operational needs.

## How it compares to pm2 (measured)

| | creekd | pm2 |
|---|---:|---:|
| Spawn → /healthz 200 (p50) | **22 ms** | 187 ms |
| Supervisor RSS (idle) | **12 MB** | 60 MB |
| Memory cap reaction | Kernel OOM, < 100 ms | Up to 30 s (poll-based) |
| Built-in HTTP router | Yes | No (use nginx) |
| Namespace / chroot isolation | Opt-in | None |

Full methodology + pro / con table: [COMPARISON.md](COMPARISON.md). Reproduce: `./up.sh && go run ./bench`.
