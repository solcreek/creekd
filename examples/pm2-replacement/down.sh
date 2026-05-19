#!/usr/bin/env bash
# Stop every app and shut creekd down. Leaves bin/ and state/ in
# place so re-running up.sh resumes quickly. Run rm -rf state bin
# if you want a fully clean slate.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

CTL="./bin/creekctl"
if [ -x "$CTL" ]; then
    for id in api worker cron; do
        $CTL rm "$id" 2>/dev/null || true
    done
fi

if [ -f creekd.pid ]; then
    pid="$(cat creekd.pid)"
    if kill -0 "$pid" 2>/dev/null; then
        kill "$pid"
        # Wait for graceful exit.
        for _ in $(seq 1 50); do
            if ! kill -0 "$pid" 2>/dev/null; then break; fi
            sleep 0.1
        done
    fi
    rm -f creekd.pid
fi
echo "stopped"
