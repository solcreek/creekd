#!/usr/bin/env bash
# Boot creekd with two tiny apps, then show the /metrics endpoint
# producing Prometheus exposition.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

mkdir -p bin
echo "==> building creekd binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"   ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)

# Detect cgroup writability so the memory metrics actually have
# something to read; non-Linux / non-root hosts still work, just
# without per-app memory.current.
LIMIT_ARGS=""
CGROUP_PARENT=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
    CGROUP_PARENT="creekd-obs-example.slice"
    LIMIT_ARGS="--memory-high 64M --memory-max 128M"
    echo "==> cgroup v2 writable: per-app memory metrics enabled"
else
    echo "==> cgroup v2 not writable: skipping per-app memory metrics"
fi

# A token is required for the /metrics endpoint — it ships through
# the same bearer-token guard as the rest of the admin API.
TOKEN="$(openssl rand -hex 16)"
echo "$TOKEN" > .token

mkdir -p state
echo "==> starting creekd"
CREEKD_STATE_DIR="$HERE/state" \
CREEKD_CGROUP_PARENT="$CGROUP_PARENT" \
CREEKD_ADMIN_TOKEN="$TOKEN" \
    ./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid
for _ in $(seq 1 50); do
    curl -sf -H "Authorization: Bearer $TOKEN" \
        http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
    sleep 0.1
done

echo "==> spawning two apps so the metrics have something to label"
export CREEKCTL_TOKEN="$TOKEN"
./bin/creekctl up app-a --command sleep --arg 300 --port 18501 $LIMIT_ARGS >/dev/null
./bin/creekctl up app-b --command sleep --arg 300 --port 18502 $LIMIT_ARGS >/dev/null

echo
echo "ready. try the metrics endpoint:"
echo
echo "  curl -H 'Authorization: Bearer \$(cat .token)' http://127.0.0.1:9080/metrics"
echo
echo "(token is in .token in this directory)"
echo
echo "tear down with: ./down.sh"
