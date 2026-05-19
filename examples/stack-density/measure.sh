#!/usr/bin/env bash
# Spawn N copies of each stack on consecutive ports, wait for /healthz
# on all, settle, then sample PSS via /proc/<pid>/smaps_rollup.
#
# Linux-only — relies on /proc/<pid>/smaps_rollup, which macOS does
# not have. On non-Linux the bench prints a notice and exits.
#
# Usage:
#   ./bootstrap.sh
#   ./measure.sh           # N=20, settle 10s (defaults)
#   N=50 SETTLE=15 ./measure.sh
#
# Output is one line per stack:
#   stack=<name>  PSS p50=<KB>  p95=<KB>  total=<KB>  ...
set -euo pipefail

if [ "$(uname -s)" != "Linux" ]; then
  echo "measure.sh needs Linux for /proc/<pid>/smaps_rollup (PSS reading)."
  echo "On macOS, ssh into a Linux host and run this there."
  exit 2
fi
if ! command -v bc >/dev/null; then
  echo "measure.sh needs \`bc\`. Install: apt-get install bc"
  exit 2
fi

HERE="$(cd "$(dirname "$0")" && pwd)"
N=${N:-20}
SETTLE=${SETTLE:-10}
BASE_PORT=${BASE_PORT:-20000}

# Each row: name | runtime | entrypoint (absolute)
STACKS=(
  "bun-hello|bun|$HERE/stacks/bun-hello/server.js"
  "hono|bun|$HERE/stacks/hono/server.js"
  "sveltekit|bun|$HERE/stacks/sveltekit/build/index.js"
  "astro|bun|$HERE/stacks/astro/dist/server/entry.mjs"
)

# Bail early if any fixture missing — pointing at clear next step.
for stack in "${STACKS[@]}"; do
  IFS='|' read -r name runtime entry <<< "$stack"
  if [ ! -f "$entry" ]; then
    echo "missing: $entry"
    echo "run ./bootstrap.sh first."
    exit 2
  fi
done

run_one() {
  local name="$1" runtime="$2" entry="$3" port_offset=$4
  local base=$((BASE_PORT + port_offset * 100))
  local pids=()

  for i in $(seq 0 $((N-1))); do
    local port=$((base + i))
    PORT=$port HOST=127.0.0.1 "$runtime" run "$entry" >/dev/null 2>&1 &
    pids+=($!)
  done

  # Wait for all /healthz to answer 200, 90s budget total.
  local healthy=0
  local deadline=$(($(date +%s) + 90))
  while [ $(date +%s) -lt $deadline ] && [ $healthy -lt $N ]; do
    healthy=0
    for i in $(seq 0 $((N-1))); do
      local port=$((base + i))
      if curl -sf "http://127.0.0.1:$port/healthz" -o /dev/null -m 2 2>/dev/null; then
        healthy=$((healthy+1))
      fi
    done
    [ $healthy -lt $N ] && sleep 1
  done

  if [ $healthy -lt $N ]; then
    echo "stack=$name  healthy=$healthy/$N  TIMEOUT"
    for pid in "${pids[@]}"; do kill $pid 2>/dev/null || true; done
    return 1
  fi

  sleep "$SETTLE"

  # Sample PSS + RSS per pid from /proc/<pid>/smaps_rollup.
  local pss_values=()
  local rss_values=()
  for pid in "${pids[@]}"; do
    local pss rss
    pss=$(awk '/^Pss:/{print $2; exit}' "/proc/$pid/smaps_rollup" 2>/dev/null || echo 0)
    rss=$(awk '/^Rss:/{print $2; exit}' "/proc/$pid/smaps_rollup" 2>/dev/null || echo 0)
    [ "$pss" -gt 0 ] && pss_values+=("$pss")
    [ "$rss" -gt 0 ] && rss_values+=("$rss")
  done

  # Percentiles via sort + index.
  local pss_sorted=($(printf '%s\n' "${pss_values[@]}" | sort -n))
  local rss_sorted=($(printf '%s\n' "${rss_values[@]}" | sort -n))
  local count=${#pss_sorted[@]}
  local p50_idx=$((count * 50 / 100))
  local p95_idx=$((count * 95 / 100))
  [ $p95_idx -ge $count ] && p95_idx=$((count-1))
  local pss_p50=${pss_sorted[$p50_idx]}
  local pss_p95=${pss_sorted[$p95_idx]}
  local rss_p50=${rss_sorted[$p50_idx]}
  local total_pss=0
  for v in "${pss_values[@]}"; do total_pss=$((total_pss + v)); done

  printf "stack=%-12s  PSS p50=%5d KB (%5.1f MB)  p95=%5d KB  RSS p50=%5d KB  total PSS=%d KB\n" \
    "$name" "$pss_p50" "$(echo "scale=1; $pss_p50/1024" | bc)" \
    "$pss_p95" "$rss_p50" "$total_pss"

  for pid in "${pids[@]}"; do kill $pid 2>/dev/null || true; done
  sleep 2
  for pid in "${pids[@]}"; do kill -9 $pid 2>/dev/null || true; done
  sleep 1
}

echo "=== stack-density bench, N=$N apps each, settle ${SETTLE}s ==="
echo "host: $(uname -srm)  bun: $(bun --version)  node: $(node --version)"
echo ""

idx=0
for stack in "${STACKS[@]}"; do
  IFS='|' read -r name runtime entry <<< "$stack"
  run_one "$name" "$runtime" "$entry" $idx || echo "  -> $name FAILED"
  idx=$((idx+1))
done
