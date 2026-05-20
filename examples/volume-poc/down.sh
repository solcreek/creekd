#!/usr/bin/env bash
# Tear down the volume-poc demo: stop the app, unmount any host
# binds under VolumeRoot, kill creekd, remove scratch dirs.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

CTL=./bin/creekctl
[ -x "$CTL" ] && $CTL rm toy 2>/dev/null || true

# Unmount any leftover binds. The Volume substrate intentionally
# leaves mounts in place across `creekd stop` (Phase 1 design — see
# RFC-stateful-substrate.md "Mounts persist by design"); the demo
# cleans them up so re-running up.sh is idempotent.
ROOTFS="$HERE/rootfs"
if [ -d "$ROOTFS" ]; then
    for m in $(awk -v p="$ROOTFS" '$5 ~ p {print $5}' /proc/self/mountinfo 2>/dev/null | sort -r); do
        umount -l "$m" 2>/dev/null || true
    done
fi
VOL_ROOT="$HERE/scratch/volumes"
if [ -d "$VOL_ROOT" ]; then
    for m in $(awk -v p="$VOL_ROOT" '$5 ~ p {print $5}' /proc/self/mountinfo 2>/dev/null | sort -r); do
        umount -l "$m" 2>/dev/null || true
    done
fi

if [ -f creekd.pid ]; then
    pid="$(cat creekd.pid)"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    rm -f creekd.pid
fi

rm -rf rootfs scratch
echo "stopped"
