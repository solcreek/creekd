#!/usr/bin/env bash
# Ensure the per-stack fixtures used by the traffic-density bench are
# built. Re-uses the fixtures owned by the two density examples next
# door instead of rebuilding them — single source of truth per stack.
#
#   - Light stacks (bun-hello, hono, sveltekit, astro): bootstrapped
#     by ../stack-density/bootstrap.sh
#   - Next.js standalone: bootstrapped by ../nextjs-density/up.sh
#
# Both bootstraps are idempotent; this script just invokes them when
# their output isn't present yet.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO_EXAMPLES="$(cd "$HERE/.." && pwd)"

if [ ! -f "$REPO_EXAMPLES/stack-density/stacks/bun-hello/server.js" ]; then
  echo "==> bootstrapping ../stack-density first (one-time, ~3 min)"
  (cd "$REPO_EXAMPLES/stack-density" && ./bootstrap.sh)
fi

if [ ! -f "$REPO_EXAMPLES/nextjs-density/app/.next/standalone/server.js" ]; then
  echo "==> bootstrapping ../nextjs-density first (one-time, ~2 min)"
  (cd "$REPO_EXAMPLES/nextjs-density" && ./up.sh)
fi

echo ""
echo "fixtures ready (5 stacks):"
ls -la \
  "$REPO_EXAMPLES/stack-density/stacks/bun-hello/server.js" \
  "$REPO_EXAMPLES/stack-density/stacks/hono/server.js" \
  "$REPO_EXAMPLES/stack-density/stacks/sveltekit/build/index.js" \
  "$REPO_EXAMPLES/stack-density/stacks/astro/dist/server/entry.mjs" \
  "$REPO_EXAMPLES/nextjs-density/app/.next/standalone/server.js" 2>/dev/null \
  | awk '{print "  ", $NF}'
echo ""
echo "run the bench:  go run ./bench"
