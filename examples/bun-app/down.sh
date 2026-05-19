#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
CTL=./bin/creekctl
[ -x "$CTL" ] && $CTL rm bun-demo 2>/dev/null || true
if [ -f creekd.pid ]; then
    pid="$(cat creekd.pid)"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    rm -f creekd.pid
fi
echo "stopped"
