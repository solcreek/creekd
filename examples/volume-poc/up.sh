#!/usr/bin/env bash
# Volume-PoC happy path:
#   1. Start creekd with a writable VolumeRoot.
#   2. Register a Volume via POST /v1/volumes.
#   3. Spawn the toy app with a VolumeMount projecting the Volume
#      into the child at /data.
#   4. Write through the bind ("hello-from-tenant"), kill the app,
#      respawn, prove the data persisted.
#
# Linux-only. Requires root (bind mounts + cgroup). On macOS run
# inside the test container:
#
#   docker build -f ../../Dockerfile.test -t creekd-test:dev ../.. \
#     && docker run --rm -it --privileged --cgroupns=host \
#     -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
#     -w /work/examples/volume-poc \
#     creekd-test:dev ./up.sh
#
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
cd "$HERE"

if [ "$(uname)" != "Linux" ]; then
    echo "This example is Linux-only (bind mount + chroot)."
    echo "On macOS run it inside the test container — see header of up.sh."
    exit 1
fi

if [ "$(id -u)" -ne 0 ]; then
    echo "This demo needs root (bind mount + cgroup). Try sudo."
    exit 1
fi

mkdir -p bin
echo "==> building binaries"
(cd "$REPO" && go build -o "$HERE/bin/creekd"   ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)
CGO_ENABLED=0 go build -ldflags='-s -w' -o "$HERE/bin/toy" ./toy

# Per-tenant volume layout. In production this would live under
# /var/lib/creekd/volumes — we use a scratch dir so the PoC is
# self-contained.
VOL_ROOT="$HERE/scratch/volumes"
mkdir -p "$VOL_ROOT/tenant-a/data"
echo "==> volume root: $VOL_ROOT"

# Build a minimal rootfs containing just the toy binary. With chroot
# set, the tenant sees ONLY this rootfs as / — the host's /etc,
# /home, etc. are invisible.
ROOTFS="$HERE/rootfs"
mkdir -p "$ROOTFS/bin" "$ROOTFS/data"
cp "$HERE/bin/toy" "$ROOTFS/bin/toy"

# Detect cgroup writability.
CGROUP_PARENT=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
    CGROUP_PARENT="creekd-volume-poc.slice"
fi

# Default the host-target allowlist permissively for the demo —
# normal deploys would set this narrowly. AllowedTargetPrefixes does
# NOT apply when Sandbox.Chroot is set (the chroot is the
# containment); we list /data anyway as the no-chroot fallback for
# the attack matrix later.
export CREEKD_VOLUME_ROOT="$VOL_ROOT"
export CREEKD_ALLOWED_TARGET_PREFIXES="/data"
export CREEKD_CGROUP_PARENT="$CGROUP_PARENT"

echo "==> starting creekd"
./bin/creekd > creekd.log 2>&1 &
echo $! > creekd.pid

# Wait for admin API.
for _ in $(seq 1 50); do
    curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
    sleep 0.1
done

ADMIN="http://127.0.0.1:9080"

echo
echo "==> registering Volume vol-a → tenant-a/data"
curl -sf -X POST "$ADMIN/v1/volumes" \
    -H 'Content-Type: application/json' \
    -d '{"id":"vol-a","backing_path":"tenant-a/data"}' | jq .

echo
echo "==> spawning toy app with vol-a mounted at /data"
# Mount the volume at /data inside the chroot. With Sandbox.Chroot
# set, the host-side bind lands at <chroot>/data — invisible to
# anything outside the chroot.
curl -sf -X POST "$ADMIN/v1/apps" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<JSON
{
  "id": "toy",
  "command": "/bin/toy",
  "port": 18200,
  "env": ["PORT=18200"],
  "sandbox": {
    "chroot": "$ROOTFS",
    "pid_namespace": true,
    "mount_namespace": true,
    "uts_namespace": true
  },
  "volume_mounts": [
    {"volume_id": "vol-a", "target": "/data"}
  ]
}
JSON
)" | jq .

# Give the toy a moment to bind its listener.
sleep 0.5

echo
echo "==> writing through the bind"
curl -sf -X POST -H 'X-Creek-App: toy' \
    "http://127.0.0.1:9000/write?content=hello-from-tenant"

echo
echo "==> reading back through the bind"
curl -sf -H 'X-Creek-App: toy' http://127.0.0.1:9000/read
echo

echo
echo "==> proving the data landed on the HOST under the volume"
echo "host path: $VOL_ROOT/tenant-a/data/marker"
cat "$VOL_ROOT/tenant-a/data/marker"
echo

echo
echo "==> restarting the toy (new PID) to prove the data survives"
./bin/creekctl restart toy
sleep 0.5
echo "after restart:"
curl -sf -H 'X-Creek-App: toy' http://127.0.0.1:9000/read
echo

echo
echo "happy path: PASS"
echo
echo "next: ./attacks.sh   # tries 11 attack vectors and asserts each is blocked"
echo "tear down: ./down.sh"
