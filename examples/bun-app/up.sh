#!/usr/bin/env bash
# Spawn a Bun HTTP app via creekd's --runtime bun path.
# Requires bun on PATH. Same go-build dance as the other examples.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

command -v bun >/dev/null || { echo "bun is required — install from https://bun.sh"; exit 1; }

mkdir -p bin
echo "==> building creekd binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"   ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)

# Detect cgroup writability; opt into memory + pids cap when available.
LIMIT_ARGS=""
CGROUP_PARENT=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
    CGROUP_PARENT="creekd-bun-example.slice"
    LIMIT_ARGS="--memory-max 128M --pids-max 64"
    echo "==> cgroup v2 writable: enforcing 128M / 64-pid caps"
else
    echo "==> cgroup v2 not writable: app runs uncapped"
fi

mkdir -p state
echo "==> starting creekd"
CREEKD_STATE_DIR="$HERE/state" \
CREEKD_CGROUP_PARENT="$CGROUP_PARENT" \
    ./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid
for _ in $(seq 1 50); do
    curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
    sleep 0.1
done

# Here's the key bit this example proves: --runtime bun --entry path
# resolves through internal/runtime/runtime.go → 'bun <entry>' invocation.
# creekd handles bun being on PATH; the user just declares their entry.
echo "==> spawning bun app"
./bin/creekctl up bun-demo \
    --runtime bun \
    --entry "$HERE/app/server.ts" \
    --env "APP_VERSION=$(git -C "$REPO" rev-parse --short HEAD 2>/dev/null || echo dev)" \
    --port 18401 \
    $LIMIT_ARGS >/dev/null

echo
echo "ready. try:"
echo "  curl -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/"
echo "  curl -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/db"
echo "  curl -H 'X-Creek-App: bun-demo' http://127.0.0.1:9000/events  # SSE — Ctrl-C to stop"
echo
echo "  ./bin/creekctl get bun-demo"
echo "  ./bin/creekctl stats bun-demo"
echo
echo "tear down with: ./down.sh"
