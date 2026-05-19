# review-apps

One creekd host, many open PRs, each with its own running review app and its own URL — no per-branch VM, no extra orchestration. Push, rebuild, `creekctl deploy`, traffic swaps with zero downtime.

This is the workflow Heroku Review Apps / Vercel Preview Deployments / Render Pull Requests give you, condensed into one daemon.

## What it shows

- Two PRs (`pr-123`, `pr-456`) running side-by-side on their own ports.
- Hostname-style routing via `X-Creek-App` — one wildcard front-door (`*.review.example.com` in production) maps every PR's URL onto creekd's dispatch listener.
- `creekctl deploy <id>` spawns the new version on a different port, health-probes it, atomically swaps the dispatch route, then drains the old version. If the new version fails the health probe within `--ready-timeout-ms`, the swap is aborted and the old version keeps serving.

## Run it

```bash
./up.sh
```

Boots creekd and two review apps:

```
==> spawning pr-123 (v1.0.0) on 18301
==> spawning pr-456 (v0.9.0) on 18302
```

Hit each:

```bash
curl -H 'X-Creek-App: pr-123' http://127.0.0.1:9000/
# review app: pr-123 (version v1.0.0, host=127.0.0.1:9000, pid=...)

curl -H 'X-Creek-App: pr-456' http://127.0.0.1:9000/
# review app: pr-456 (version v0.9.0, host=127.0.0.1:9000, pid=...)
```

### Demo the swap

CI just rebuilt pr-123 at v2.0.0. Roll it out:

```bash
./redeploy.sh pr-123 v2.0.0

# response now shows v2.0.0 — without a single dropped connection
curl -H 'X-Creek-App: pr-123' http://127.0.0.1:9000/
# review app: pr-123 (version v2.0.0, host=127.0.0.1:9000, pid=<new>)
```

`creekctl deploy` did the blue-green dance:

1. spawn pr-123 v2.0.0 on port 18311 (the alt port; the script flips between 18301↔18311 each redeploy)
2. probe `http://127.0.0.1:18311/healthz` until 200 (with a 5s timeout)
3. atomically swap the dispatch route: pr-123 → 18311
4. stop the old (v1.0.0) instance on 18301

If step 2 had failed, none of 3–4 would have happened — the v1.0.0 instance keeps serving.

### List + tear down

```bash
./bin/creekctl ps
# ID       STATUS   PID    PORT   UPTIME      RESTARTS
# pr-123   running  ...    18311  10s         0
# pr-456   running  ...    18302  35s         0

./down.sh
```

## Wiring it into real CI

The redeploy command is one creekctl call:

```bash
creekctl deploy "pr-${PR_NUMBER}" \
    --command "$BUILD_OUTPUT/server" \
    --env "APP_NAME=pr-${PR_NUMBER}" --env "APP_VERSION=${COMMIT_SHA}" \
    --env "PORT=${PORT}" --port "${PORT}" \
    --ready-timeout-ms 30000
```

GitHub Actions / GitLab CI would:

1. Build the PR's binary in CI.
2. `scp` or `rsync` the binary to the creekd host.
3. Run the `creekctl deploy` above against the host's admin API (with `CREEKCTL_SERVER` and `CREEKCTL_TOKEN` from secrets).
4. Post a comment back to the PR with the URL: `https://pr-${PR_NUMBER}.review.example.com`.

The DNS side is a one-time setup: `*.review.example.com → ${CREEKD_HOST}`, plus a reverse proxy (Caddy / nginx / Cloudflare) in front of the creekd dispatch listener that copies `Host` into `X-Creek-App` so creekctl-spawned IDs match the subdomain.

## What this doesn't cover

- **Build pipeline.** This example assumes the binary already exists. Phase 1 of creekd is the runtime layer; the build side is yours or the wider Creek stack's.
- **Per-PR isolation between review apps.** They share the host's filesystem and network namespace unless you add `--chroot` / `--pid-namespace` / etc. — see the sandboxed-eval example. For internal-team review apps, the shared model is usually what you want.
- **TLS.** Put Caddy or Cloudflare in front. Caddy with `tls internal` will auto-issue certs for `*.review.example.com` if you can give it the DNS-01 challenge.
- **Multi-host.** One creekd, one box. For very high PR volume, partition by team / repo across multiple creekd hosts.
