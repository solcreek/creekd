#!/usr/bin/env bash
# Boot a local creekd and spawn three toy apps, demonstrating the
# pm2 + nginx + manual-process-management workflow collapsed into
# one daemon + one CLI.
#
# Run from this directory:  ./up.sh
# Tear down with:           ./down.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

# Build creekd + creekctl + the toy app (idempotent — go skips
# unchanged inputs).
mkdir -p bin
echo "==> building binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"  ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)
go build -o "$HERE/bin/toy" ./toy

# Detect whether we can actually enforce cgroup limits. v0.1.0 needs
# Linux + cgroup v2 + write access to a parent slice — typically root
# on a Linux box. On macOS dev hosts or unprivileged Linux, we skip
# the cap flags and let the apps run uncapped (everything else still
# works — routing, restart, dispatch).
LIMIT_ARGS=""
CGROUP_PARENT=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
    CGROUP_PARENT="creekd-example.slice"
    LIMIT_ARGS="--memory-max 64M --pids-max 32"
    echo "==> cgroup v2 writable: enforcing memory / pids caps"
else
    echo "==> cgroup v2 not writable (macOS dev, or no root): apps run uncapped"
fi

# Boot creekd in the background. State dir lets the example survive
# accidentally re-running up.sh (apps come back from state.json
# instead of erroring with "already exists").
mkdir -p state
echo "==> starting creekd (logs → creekd.log)"
CREEKD_STATE_DIR="$HERE/state" \
CREEKD_CGROUP_PARENT="$CGROUP_PARENT" \
    ./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid

# Wait for the admin listener to come up.
for _ in $(seq 1 50); do
    if curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1; then
        break
    fi
    sleep 0.1
done

CTL="./bin/creekctl"

# Three apps on three distinct ports, each with its own identity
# via APP_NAME. Memory caps are conservative — these are tiny.
echo "==> spawning api (port 18001)"
$CTL up api \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=api" --env "PORT=18001" --port 18001 \
    $LIMIT_ARGS >/dev/null

echo "==> spawning worker (port 18002)"
$CTL up worker \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=worker" --env "PORT=18002" --port 18002 \
    $LIMIT_ARGS >/dev/null

echo "==> spawning cron (port 18003)"
$CTL up cron \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=cron" --env "PORT=18003" --port 18003 \
    $LIMIT_ARGS >/dev/null

echo
echo "ready. try:"
echo "  curl -H 'X-Creek-App: api'    http://127.0.0.1:9000/"
echo "  curl -H 'X-Creek-App: worker' http://127.0.0.1:9000/"
echo "  curl -H 'X-Creek-App: cron'   http://127.0.0.1:9000/"
echo
echo "list:    $CTL ps"
echo "logs:    $CTL logs api"
echo "stats:   $CTL stats api"
echo "restart: $CTL restart api"
echo
echo "to demo auto-restart on crash:"
echo "  curl -H 'X-Creek-App: api' http://127.0.0.1:9000/crash"
echo "  $CTL get api    # restart_count goes up"
