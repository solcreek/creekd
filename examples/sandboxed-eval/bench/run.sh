#!/usr/bin/env bash
# Build the bench image + toy image on the host, start creekd
# inside a privileged Linux container, run the bench against both
# creekd-sandbox and docker (host daemon via mounted socket).
#
# Key detail: the source tree is bind-mounted to the SAME path
# inside the container that it lives at on the host. That way
# docker commands launched from inside (going back through the
# socket to the host daemon) reference paths the host can resolve.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
EXDIR="$(cd "$HERE/.." && pwd)"
REPO="$(cd "$EXDIR/../.." && pwd)"

HARNESS=creekd-sandbox-bench:dev
TOY_IMG=creekd-sandbox-bench-toy:latest

echo "==> building harness image"
docker build -q -f "$HERE/Dockerfile" -t "$HARNESS" "$REPO" >/dev/null

# The toy image baking has to happen *after* up.sh produces a
# static bin/toy, but on the host so docker build's context is
# host-visible. We run a throwaway harness container just to do
# the up.sh build step, then build the toy image, then run the
# main bench container.
echo "==> producing static toy binary"
docker run --rm \
    -v "$REPO:$REPO" \
    -w "$EXDIR" \
    "$HARNESS" \
    bash -c 'CGO_ENABLED=0 go build -ldflags="-s -w" -o bin/toy ./toy'

echo "==> building toy image"
TOY_CTX="$(mktemp -d)"
trap 'rm -rf "$TOY_CTX"' EXIT
cp "$EXDIR/bin/toy" "$TOY_CTX/toy"
cat > "$TOY_CTX/Dockerfile" <<EOF
FROM scratch
COPY toy /toy
ENTRYPOINT ["/toy"]
EOF
docker build -q -t "$TOY_IMG" "$TOY_CTX" >/dev/null

echo "==> running bench"
docker run --rm \
    --privileged \
    --cgroupns=host \
    --network=host \
    -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    -v /var/run/docker.sock:/var/run/docker.sock \
    -v "$REPO:$REPO" \
    -w "$EXDIR" \
    -e BENCH_TOY_IMG="$TOY_IMG" \
    "$HARNESS" \
    bash -c './up.sh >/dev/null && sleep 1 && go run ./bench -n "${BENCH_N:-10}" ; ./down.sh >/dev/null 2>&1'
