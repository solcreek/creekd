#!/usr/bin/env bash
# Stop and remove every container the bench produced. Idempotent.
set -euo pipefail

# Containers are named bench-bare-* and bench-docker-* by the harness.
for prefix in bench-bare bench-docker bench-creekd; do
    ids=$(docker ps -aq --filter "name=^${prefix}-" 2>/dev/null || true)
    if [ -n "$ids" ]; then
        echo "==> stopping ${prefix}-* containers"
        docker rm -f $ids >/dev/null
    fi
done

# Bare bun processes the harness might have orphaned (matched by
# environment variable PORT range 19200..19299 — see bench/main.go).
pkill -f "bun .*server.js.*BENCH_BARE" 2>/dev/null || true

echo "done."
