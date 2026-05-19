#!/usr/bin/env bash
# Prepare the density bench:
#   1. Build the Next.js fixture (produces app/.next/standalone/).
#   2. Build the Docker image around that standalone tree.
#
# Run from this directory:  ./up.sh
# Tear down with:           ./down.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

echo "==> building Next.js fixture"
(cd app && pnpm install --silent && pnpm build)

echo "==> building Docker image (creekd-nextjs-density)"
docker build -q -t creekd-nextjs-density:bench .

echo
echo "ready. run the bench:"
echo "  go run ./bench -n 10                # 10 apps per scenario"
echo "  go run ./bench -n 50 -scenario docker  # docker only"
