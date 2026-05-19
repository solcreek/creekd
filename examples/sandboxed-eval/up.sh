#!/usr/bin/env bash
# Demo: spawn a sandboxed app inside a chroot + PID namespace +
# user namespace + NoNewPrivs, then poke at it from the host.
#
# Linux-only. Requires either root or capabilities for cgroup +
# namespace + chroot. On macOS run this inside the test container:
#
#   docker build -f ../../Dockerfile.test -t creekd-test:dev ../.. \
#     && docker run --rm -it --privileged --cgroupns=host \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
#     -w /work/examples/sandboxed-eval \
#     creekd-test:dev ./up.sh
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

if [ "$(uname)" != "Linux" ]; then
    echo "This example is Linux-only (namespace + chroot + cgroup primitives)."
    echo "On macOS run it inside the test container — see the header of up.sh."
    exit 1
fi

mkdir -p bin
echo "==> building binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"   ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)
# Static toy: lives inside the chroot, can't depend on libc.so.
CGO_ENABLED=0 go build -ldflags='-s -w' -o "$HERE/bin/toy" ./toy

# Build a minimal rootfs containing only the toy binary. Inside the
# sandbox this is /; the host's /etc, /home, etc. are invisible.
ROOTFS="$HERE/rootfs"
mkdir -p "$ROOTFS/bin"
cp "$HERE/bin/toy" "$ROOTFS/bin/toy"

# Detect cgroup writability. On real Linux + root this works; in
# unprivileged contexts we'll spawn without cgroup caps.
LIMIT_ARGS=""
CGROUP_PARENT=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
    CGROUP_PARENT="creekd-sandbox.slice"
    LIMIT_ARGS="--memory-max 64M --pids-max 32"
fi

echo "==> starting creekd"
CREEKD_CGROUP_PARENT="$CGROUP_PARENT" ./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid
for _ in $(seq 1 50); do
    curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
    sleep 0.1
done

CTL=./bin/creekctl

# Sandbox combo:
#   --chroot           → only /bin/toy is visible inside
#   --pid-namespace    → child sees itself as pid 1
#   --mount-namespace  → mount table is its own copy
#   --uts-namespace    → hostname is independent
#
# --no-new-privs is omitted here: the v0.1.0 wrap calls /usr/bin/setpriv
# which the kernel looks for INSIDE the chroot (because chroot is
# applied before exec). To use both, copy setpriv + its shared libs
# into the rootfs. The supervisor will move to an inline prctl path
# in a later release; see sandbox_linux.go: WrapNoNewPrivs.
echo "==> spawning sandboxed eval app"
$CTL up eval \
    --command "/bin/toy" \
    --env "PORT=18100" --port 18100 \
    --chroot "$ROOTFS" \
    --pid-namespace --mount-namespace --uts-namespace \
    $LIMIT_ARGS >/dev/null

echo
echo "ready. try:"
echo
echo "  # baseline identity"
echo "  curl -H 'X-Creek-App: eval' http://127.0.0.1:9000/"
echo
echo "  # prove the chroot — should ONLY see /bin/toy, no /etc/passwd"
echo "  curl -H 'X-Creek-App: eval' http://127.0.0.1:9000/view"
echo
if [ -n "$LIMIT_ARGS" ]; then
    echo "  # trip the 64 MiB cgroup cap (kernel OOM kills the process)"
    echo "  curl -H 'X-Creek-App: eval' 'http://127.0.0.1:9000/alloc?mb=128'"
    echo "  $CTL get eval   # restart_count should bump"
fi
echo
echo "tear down with: ./down.sh"
