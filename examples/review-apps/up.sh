#!/usr/bin/env bash
# Stand up review apps for two PRs and demo a zero-downtime swap.
#
#   ./up.sh                 boot creekd + two review apps (pr-123, pr-456)
#   ./redeploy.sh pr-123    blue-green deploy a new build of pr-123
#   ./down.sh               tear it all down
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

mkdir -p bin
echo "==> building binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"   ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)
go build -o "$HERE/bin/toy" ./toy

mkdir -p state
echo "==> starting creekd (state dir + log)"
CREEKD_STATE_DIR="$HERE/state" ./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid
for _ in $(seq 1 50); do
    curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
    sleep 0.1
done

CTL=./bin/creekctl

# Each PR's review app lives on its own port. The hostname-style
# convention (pr-123, pr-456) maps onto X-Creek-App for dispatch;
# in production you'd front this with a wildcard subdomain like
# *.review.example.com → 9000 and let creekd route on Host.
echo "==> spawning pr-123 (v1.0.0) on 18301"
$CTL up pr-123 \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=pr-123" --env "APP_VERSION=v1.0.0" --env "PORT=18301" \
    --port 18301 >/dev/null

echo "==> spawning pr-456 (v0.9.0) on 18302"
$CTL up pr-456 \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=pr-456" --env "APP_VERSION=v0.9.0" --env "PORT=18302" \
    --port 18302 >/dev/null

echo
echo "ready. try:"
echo "  curl -H 'X-Creek-App: pr-123' http://127.0.0.1:9000/"
echo "  curl -H 'X-Creek-App: pr-456' http://127.0.0.1:9000/"
echo
echo "demo a zero-downtime swap of pr-123 to v2.0.0:"
echo "  ./redeploy.sh pr-123 v2.0.0"
echo
echo "tear down with: ./down.sh"
