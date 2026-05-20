#!/usr/bin/env bash
# Attack matrix for the volume-mount substrate. Each case sends a
# crafted admin-API request that a compromised orchestrator might
# send and asserts the supervisor refuses with the expected error.
#
# Run AFTER up.sh — depends on creekd being live with the vol-a
# volume already registered.
#
# Each attack is labeled to the pentest finding it covers (see
# docs/RFC-stateful-substrate.md follow-up review notes).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

if [ ! -f creekd.pid ] || ! kill -0 "$(cat creekd.pid)" 2>/dev/null; then
    echo "creekd not running. Run ./up.sh first."
    exit 1
fi

ADMIN="http://127.0.0.1:9080"
PASS=0
FAIL=0

# expect_block <label> <expected-error-substring> <body>
# Sends body to POST /v1/apps, asserts 4xx, asserts the error message
# contains the expected substring.
expect_block() {
    local label="$1"
    local needle="$2"
    local body="$3"
    local app_id
    app_id="atk-$(echo "$label" | tr ' /[:upper:]' '-[:lower:]' | tr -cd 'a-z0-9-')"
    body="$(echo "$body" | sed "s/__ID__/$app_id/g")"

    local out status
    out="$(curl -s -o /tmp/creekd-attack.body -w '%{http_code}' \
              -X POST "$ADMIN/v1/apps" \
              -H 'Content-Type: application/json' -d "$body")"
    status="$out"
    local resp
    resp="$(cat /tmp/creekd-attack.body)"

    if [ "$status" -ge 400 ] && [ "$status" -lt 500 ] \
       && echo "$resp" | grep -qi "$needle"; then
        echo "  PASS — $label"
        echo "         server returned $status: $resp" | head -c 200 | sed 's/$/\n/'
        PASS=$((PASS+1))
    else
        echo "  FAIL — $label"
        echo "         expected 4xx with \"$needle\", got $status: $resp"
        FAIL=$((FAIL+1))
        # Best-effort cleanup if the spawn succeeded.
        ./bin/creekctl rm "$app_id" 2>/dev/null || true
    fi
}

# expect_block_volume <label> <expected-error> <body>
# Same shape but targets POST /v1/volumes.
expect_block_volume() {
    local label="$1"
    local needle="$2"
    local body="$3"
    local out status resp
    out="$(curl -s -o /tmp/creekd-attack.body -w '%{http_code}' \
              -X POST "$ADMIN/v1/volumes" \
              -H 'Content-Type: application/json' -d "$body")"
    status="$out"
    resp="$(cat /tmp/creekd-attack.body)"

    if [ "$status" -ge 400 ] && [ "$status" -lt 500 ] \
       && echo "$resp" | grep -qi "$needle"; then
        echo "  PASS — $label"
        echo "         server returned $status: $resp" | head -c 200 | sed 's/$/\n/'
        PASS=$((PASS+1))
    else
        echo "  FAIL — $label"
        echo "         expected 4xx with \"$needle\", got $status: $resp"
        FAIL=$((FAIL+1))
    fi
}

echo "======================================================================"
echo "  Volume substrate — attack matrix"
echo "  Each test sends a crafted admin-API request; supervisor MUST refuse."
echo "======================================================================"

# ---------- Path-traversal class ----------

echo
echo "[A1] BackingPath with leading '..' should be rejected at API"
expect_block_volume "A1-volume-dotdot" "contains '..'" \
    '{"id":"escape","backing_path":"../etc"}'

echo
echo "[A2] BackingPath that is absolute should be rejected"
expect_block_volume "A2-volume-absolute" "relative" \
    '{"id":"escape","backing_path":"/etc/passwd"}'

echo
echo "[A3] BackingPath that is a symlink — openat2 RESOLVE_NO_SYMLINKS"
ROOTFS="$HERE/rootfs"
ln -sfn /etc "$HERE/scratch/volumes/evil-symlink" 2>/dev/null || true
expect_block_volume "A3-symlink-escape" "ENOENT\|backing path does not exist\|ELOOP" \
    '{"id":"evil","backing_path":"evil-symlink"}'
rm -f "$HERE/scratch/volumes/evil-symlink"

# ---------- Target overlay class ----------

echo
echo "[A4] VolumeMount Target='/etc' (no chroot) — host overlay attempt"
expect_block "A4-target-system-path" "forbidden\|not under any allowed prefix\|escape" \
    '{
      "id":"__ID__","command":"/bin/true","port":18301,
      "volume_mounts":[{"volume_id":"vol-a","target":"/etc/passwd"}]
    }'

echo
echo "[A5] VolumeMount Target='/proc/something' — must be hard-denied"
expect_block "A5-target-proc" "forbidden\|not under any allowed prefix" \
    '{
      "id":"__ID__","command":"/bin/true","port":18302,
      "volume_mounts":[{"volume_id":"vol-a","target":"/proc/anything"}]
    }'

echo
echo "[A6] Sandbox.Chroot='/' (would bypass AllowedTargetPrefixes)"
expect_block "A6-chroot-slash" "chroot must not be" \
    '{
      "id":"__ID__","command":"/bin/true","port":18303,
      "sandbox":{"chroot":"/"},
      "volume_mounts":[{"volume_id":"vol-a","target":"/data"}]
    }'

# ---------- SubPath class ----------

echo
echo "[A7] SubPath with '..' — must be rejected"
expect_block "A7-subpath-dotdot" "contains '..'\|sub_path" \
    '{
      "id":"__ID__","command":"/bin/true","port":18304,
      "sandbox":{"chroot":"'"$ROOTFS"'","mount_namespace":true},
      "volume_mounts":[{"volume_id":"vol-a","sub_path":"../neighbor","target":"/data"}]
    }'

echo
echo "[A8] SubPath that is absolute"
expect_block "A8-subpath-absolute" "sub_path\|relative\|absolute" \
    '{
      "id":"__ID__","command":"/bin/true","port":18305,
      "sandbox":{"chroot":"'"$ROOTFS"'","mount_namespace":true},
      "volume_mounts":[{"volume_id":"vol-a","sub_path":"/abs","target":"/data"}]
    }'

# ---------- Reference / lifecycle class ----------

echo
echo "[A9] VolumeMount referencing an unregistered Volume"
expect_block "A9-missing-volume" "volume not found" \
    '{
      "id":"__ID__","command":"/bin/true","port":18306,
      "sandbox":{"chroot":"'"$ROOTFS"'","mount_namespace":true},
      "volume_mounts":[{"volume_id":"ghost","target":"/data"}]
    }'

echo
echo "[A10] UnregisterVolume of an in-use volume without force"
out="$(curl -s -o /tmp/creekd-attack.body -w '%{http_code}' \
       -X DELETE "$ADMIN/v1/volumes/vol-a")"
resp="$(cat /tmp/creekd-attack.body)"
if [ "$out" = "409" ] && echo "$resp" | grep -qi "still referenced\|in use"; then
    echo "  PASS — A10-delete-in-use"
    echo "         server returned $out: $resp" | head -c 200 | sed 's/$/\n/'
    PASS=$((PASS+1))
else
    echo "  FAIL — A10-delete-in-use"
    echo "         expected 409 with \"still referenced\", got $out: $resp"
    FAIL=$((FAIL+1))
fi

# ---------- Duplicate-target class ----------

echo
echo "[A11] Duplicate Target within one VolumeMounts list"
expect_block "A11-dup-target" "duplicate target" \
    '{
      "id":"__ID__","command":"/bin/true","port":18307,
      "sandbox":{"chroot":"'"$ROOTFS"'","mount_namespace":true},
      "volume_mounts":[
        {"volume_id":"vol-a","target":"/data"},
        {"volume_id":"vol-a","target":"/data"}
      ]
    }'

# ---------- Live data-confinement check ----------

echo
echo "[A12] LIVE: the toy in its chroot CANNOT read /etc/passwd off the host"
resp="$(curl -sf -H 'X-Creek-App: toy' \
    'http://127.0.0.1:9000/probe-host?path=/etc/passwd' || echo CURL_FAILED)"
if echo "$resp" | grep -qi "blocked:\|no such file"; then
    echo "  PASS — A12-host-etc-passwd-blocked"
    echo "         toy: $resp"
    PASS=$((PASS+1))
else
    echo "  FAIL — A12-host-etc-passwd-blocked"
    echo "         toy returned: $resp"
    FAIL=$((FAIL+1))
fi

echo
echo "======================================================================"
echo "  Attack matrix: $PASS passed, $FAIL failed"
echo "======================================================================"

if [ "$FAIL" -ne 0 ]; then
    echo
    echo "One or more attacks were NOT blocked. Inspect creekd.log and the"
    echo "FAIL lines above. Do not ship in this state."
    exit 1
fi
