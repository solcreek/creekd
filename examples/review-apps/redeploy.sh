#!/usr/bin/env bash
# Blue-green redeploy of a review app to a new version.
# Usage: ./redeploy.sh <app-id> <version>
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

ID="${1:?usage: $0 <app-id> <version>}"
VERSION="${2:?usage: $0 <app-id> <version>}"

CTL=./bin/creekctl

# Pick a v2 port that doesn't collide. We round-robin between two
# fixed ports per app so consecutive redeploys swap A→B→A→B; in a
# real CI you'd pull this from a port allocator.
CURRENT_PORT="$($CTL get "$ID" --json | grep '"port"' | head -1 | sed 's/[^0-9]//g')"
case "$ID" in
    pr-123) ALT_A=18301; ALT_B=18311 ;;
    pr-456) ALT_A=18302; ALT_B=18312 ;;
    *)      echo "unknown app id $ID"; exit 1 ;;
esac
NEXT_PORT="$ALT_A"
if [ "$CURRENT_PORT" = "$ALT_A" ]; then NEXT_PORT="$ALT_B"; fi

echo "==> redeploying $ID: port $CURRENT_PORT → $NEXT_PORT, version → $VERSION"
$CTL deploy "$ID" \
    --command "$HERE/bin/toy" \
    --env "APP_NAME=$ID" --env "APP_VERSION=$VERSION" --env "PORT=$NEXT_PORT" \
    --port "$NEXT_PORT" \
    --ready-timeout-ms 5000 >/dev/null

echo "==> swap complete; verify:"
echo "    curl -H 'X-Creek-App: $ID' http://127.0.0.1:9000/"
