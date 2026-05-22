#!/usr/bin/env bash
# M5 Soak Test — 24-hour production readiness
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../.." && pwd)"
TENANTS=${TENANTS:-50}
DURATION=${DURATION:-24h}
BUN_RATIO=${BUN_RATIO:-60}  # percent Bun (rest = Node)
BASE_PORT=19001
MEMORY_CAP=${MEMORY_CAP:-64M}
METRICS_FILE="$HERE/soak-metrics.ndjson"
RESULTS_FILE="$HERE/RESULTS.md"

log() { echo "[$(date +%H:%M:%S)] $*"; }

# --- Build ---
log "Building binaries..."
mkdir -p "$HERE/bin"
(cd "$REPO" && go build -o "$HERE/bin/creekd"  ./cmd/creekd)
(cd "$REPO" && go build -o "$HERE/bin/creekctl" ./cmd/creekctl)
log "Built creekd + creekctl"

# --- Detect cgroup ---
CGROUP_PARENT=""
LIMIT_ARGS=""
if [ -w /sys/fs/cgroup/cgroup.subtree_control ] 2>/dev/null; then
  CGROUP_PARENT="creekd-soak.slice"
  LIMIT_ARGS="--memory-max $MEMORY_CAP --pids-max 64"
  log "cgroup v2 writable: enforcing $MEMORY_CAP per tenant"
else
  log "cgroup v2 not available: running uncapped"
fi

# --- Start creekd ---
mkdir -p "$HERE/state" "$HERE/logs"
log "Starting creekd..."

CREEKD_STATE_DIR="$HERE/state" \
CREEKD_LOG_DIR="$HERE/logs" \
CREEKD_CGROUP_PARENT="$CGROUP_PARENT" \
  "$HERE/bin/creekd" > "$HERE/creekd.log" 2>&1 &
echo $! > "$HERE/creekd.pid"

for _ in $(seq 1 50); do
  curl -sf http://127.0.0.1:9080/v1/apps >/dev/null 2>&1 && break
  sleep 0.1
done
log "creekd ready (PID $(cat "$HERE/creekd.pid"))"

CTL="$HERE/bin/creekctl"
TENANT_APP="$HERE/tenant/index.mjs"

# --- Detect runtimes ---
BUN_CMD=$(which bun 2>/dev/null || echo "")
NODE_CMD=$(which node 2>/dev/null || echo "")
if [ -z "$BUN_CMD" ] && [ -z "$NODE_CMD" ]; then
  log "ERROR: neither bun nor node found"
  exit 1
fi

# --- Phase 1: Spawn tenants ---
log "Spawning $TENANTS tenants (${BUN_RATIO}% Bun)..."
BUN_COUNT=$(( TENANTS * BUN_RATIO / 100 ))
SPAWN_START=$(date +%s)

for i in $(seq 1 "$TENANTS"); do
  PORT=$(( BASE_PORT + i - 1 ))
  ID=$(printf "tenant-%03d" "$i")

  if [ "$i" -le "$BUN_COUNT" ] && [ -n "$BUN_CMD" ]; then
    RUNTIME_CMD="$BUN_CMD"
    RUNTIME_NAME="bun"
  else
    RUNTIME_CMD="$NODE_CMD"
    RUNTIME_NAME="node"
  fi

  $CTL ensure "$ID" \
    --command "$RUNTIME_CMD" --arg "$TENANT_APP" \
    --env "APP_ID=$ID" --env "PORT=$PORT" --port "$PORT" \
    --health-path "/health" \
    $LIMIT_ARGS \
    --json > /dev/null 2>&1

  if [ $(( i % 10 )) -eq 0 ]; then
    log "  spawned $i/$TENANTS"
  fi
done

SPAWN_SECS=$(( $(date +%s) - SPAWN_START ))
RUNNING=$($CTL ps --json 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "?")
log "Phase 1 done: $RUNNING/$TENANTS running in ${SPAWN_SECS}s"

# --- Parse duration ---
parse_duration() {
  local d="$1"
  case "$d" in
    *h) echo $(( ${d%h} * 3600 )) ;;
    *m) echo $(( ${d%m} * 60 )) ;;
    *s) echo "${d%s}" ;;
    *)  echo "$d" ;;
  esac
}
DURATION_SECS=$(parse_duration "$DURATION")
END_TIME=$(( $(date +%s) + DURATION_SECS ))

log "Phase 2: Soak for $DURATION ($DURATION_SECS seconds)..."

# --- Phase 2: Continuous monitoring + fault injection ---
CRASH_INTERVAL=300  # crash a random tenant every 5 min
DEPLOY_AT=$(( $(date +%s) + DURATION_SECS / 2 ))  # deploy mid-test
DAEMON_RESTART_AT=$(( $(date +%s) + DURATION_SECS * 3 / 4 ))  # restart at 75%
LAST_CRASH=0
DEPLOYED=false
DAEMON_RESTARTED=false
METRIC_INTERVAL=60

> "$METRICS_FILE"

while [ "$(date +%s)" -lt "$END_TIME" ]; do
  NOW=$(date +%s)

  # --- Probe all tenants ---
  HEALTHY=0
  UNHEALTHY=0
  for i in $(seq 1 "$TENANTS"); do
    PORT=$(( BASE_PORT + i - 1 ))
    if curl -sf --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
      HEALTHY=$(( HEALTHY + 1 ))
    else
      UNHEALTHY=$(( UNHEALTHY + 1 ))
    fi
  done

  # --- creekd RSS (cross-platform) ---
  CREEKD_PID=$(cat "$HERE/creekd.pid" 2>/dev/null || echo "")
  CREEKD_RSS=0
  if [ -n "$CREEKD_PID" ]; then
    if [ -f "/proc/$CREEKD_PID/status" ]; then
      CREEKD_RSS=$(grep VmRSS "/proc/$CREEKD_PID/status" 2>/dev/null | awk '{print $2}' || echo 0)
    else
      # macOS: ps reports RSS in KB
      CREEKD_RSS=$(ps -o rss= -p "$CREEKD_PID" 2>/dev/null | tr -d ' ' || echo 0)
    fi
  fi

  # --- Emit metric ---
  echo "{\"ts\":$NOW,\"healthy\":$HEALTHY,\"unhealthy\":$UNHEALTHY,\"tenants\":$TENANTS,\"creekd_rss_kb\":$CREEKD_RSS}" >> "$METRICS_FILE"

  if [ $(( NOW % METRIC_INTERVAL )) -lt 5 ]; then
    log "  healthy=$HEALTHY/$TENANTS creekd_rss=${CREEKD_RSS}KB"
  fi

  # --- Fault: crash a random tenant every CRASH_INTERVAL ---
  if [ $(( NOW - LAST_CRASH )) -ge $CRASH_INTERVAL ]; then
    VICTIM_PORT=$(( BASE_PORT + RANDOM % TENANTS ))
    curl -sf --max-time 2 "http://127.0.0.1:$VICTIM_PORT/crash" >/dev/null 2>&1 || true
    VICTIM_ID=$(printf "tenant-%03d" $(( VICTIM_PORT - BASE_PORT + 1 )))
    log "  FAULT: crashed $VICTIM_ID (port $VICTIM_PORT)"

    # Measure restart time
    RESTART_START=$(date +%s%N)
    for _ in $(seq 1 100); do
      if curl -sf --max-time 1 "http://127.0.0.1:$VICTIM_PORT/health" >/dev/null 2>&1; then
        RESTART_NS=$(( $(date +%s%N) - RESTART_START ))
        RESTART_MS=$(( RESTART_NS / 1000000 ))
        log "  RESTART: $VICTIM_ID recovered in ${RESTART_MS}ms"
        echo "{\"ts\":$NOW,\"event\":\"restart\",\"id\":\"$VICTIM_ID\",\"restart_ms\":$RESTART_MS}" >> "$METRICS_FILE"
        break
      fi
      sleep 0.1
    done
    LAST_CRASH=$NOW
  fi

  # --- Fault: restart a tenant mid-test (simulates deploy) ---
  if [ "$NOW" -ge "$DEPLOY_AT" ] && [ "$DEPLOYED" = false ]; then
    DEPLOYED=true
    log "  FAULT: restart tenant-001 (simulates deploy)..."
    DEPLOY_START=$(date +%s%N)
    $CTL restart tenant-001 --json > /dev/null 2>&1 && {
      # Wait for healthy
      for _ in $(seq 1 50); do
        if curl -sf --max-time 1 "http://127.0.0.1:$BASE_PORT/health" >/dev/null 2>&1; then
          DEPLOY_NS=$(( $(date +%s%N) - DEPLOY_START ))
          DEPLOY_MS=$(( DEPLOY_NS / 1000000 ))
          log "  DEPLOY: tenant-001 restarted in ${DEPLOY_MS}ms"
          echo "{\"ts\":$NOW,\"event\":\"deploy\",\"deploy_ms\":$DEPLOY_MS}" >> "$METRICS_FILE"
          break
        fi
        sleep 0.1
      done
    } || log "  DEPLOY: restart failed (check creekd.log)"
  fi

  # --- Fault: daemon restart at 75% ---
  if [ "$NOW" -ge "$DAEMON_RESTART_AT" ] && [ "$DAEMON_RESTARTED" = false ]; then
    DAEMON_RESTARTED=true
    log "  FAULT: killing creekd (testing state restore)..."
    kill "$(cat "$HERE/creekd.pid")" 2>/dev/null || true
    sleep 2

    CREEKD_STATE_DIR="$HERE/state" \
    CREEKD_LOG_DIR="$HERE/logs" \
    CREEKD_CGROUP_PARENT="$CGROUP_PARENT" \
      "$HERE/bin/creekd" > "$HERE/creekd.log" 2>&1 &
    echo $! > "$HERE/creekd.pid"

    sleep 5
    RESTORED=$($CTL ps --json 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin)))" 2>/dev/null || echo "0")
    log "  RESTORE: $RESTORED/$TENANTS apps restored from state"
    echo "{\"ts\":$NOW,\"event\":\"daemon_restart\",\"restored\":$RESTORED}" >> "$METRICS_FILE"
  fi

  sleep 5
done

# --- Phase 3: Final verification ---
log "Phase 3: Final verification..."

FINAL_HEALTHY=0
for i in $(seq 1 "$TENANTS"); do
  PORT=$(( BASE_PORT + i - 1 ))
  if curl -sf --max-time 2 "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    FINAL_HEALTHY=$(( FINAL_HEALTHY + 1 ))
  fi
done

FINAL_RSS=0
CREEKD_PID=$(cat "$HERE/creekd.pid" 2>/dev/null || echo "")
if [ -n "$CREEKD_PID" ]; then
  if [ -f "/proc/$CREEKD_PID/status" ]; then
    FINAL_RSS=$(grep VmRSS "/proc/$CREEKD_PID/status" 2>/dev/null | awk '{print $2}' || echo 0)
  else
    FINAL_RSS=$(ps -o rss= -p "$CREEKD_PID" 2>/dev/null | tr -d ' ' || echo 0)
  fi
fi

FIRST_RSS=$(head -1 "$METRICS_FILE" | python3 -c "import json,sys; print(json.load(sys.stdin).get('creekd_rss_kb',0))" 2>/dev/null || echo 0)
RSS_DELTA=$(( FINAL_RSS - FIRST_RSS ))

RESTART_TIMES=$(grep '"restart"' "$METRICS_FILE" | python3 -c "
import json,sys
times = [json.loads(l)['restart_ms'] for l in sys.stdin]
if times:
    print(f'min={min(times)}ms max={max(times)}ms avg={sum(times)//len(times)}ms count={len(times)}')
else:
    print('no restarts recorded')
" 2>/dev/null || echo "parse error")

# --- Write results ---
cat > "$RESULTS_FILE" << EORESULTS
# M5 Soak Test Results

Date: $(date -u +%Y-%m-%dT%H:%M:%SZ)
Duration: $DURATION ($DURATION_SECS seconds)
Tenants: $TENANTS (${BUN_RATIO}% Bun, $(( 100 - BUN_RATIO ))% Node)
Host: $(uname -r) / $(nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo "?") CPU / $(free -m 2>/dev/null | awk '/Mem:/{print $2}' || sysctl -n hw.memsize 2>/dev/null | awk '{print int($1/1048576)}' || echo "?")MB RAM

## Results

| Criteria | Target | Actual | Pass? |
|---|---|---|---|
| Tenants running at end | $TENANTS | $FINAL_HEALTHY | $([ "$FINAL_HEALTHY" -ge "$TENANTS" ] && echo "✅" || echo "❌") |
| Restart latency | < 500ms | $RESTART_TIMES | |
| creekd RSS delta | < 10MB | ${RSS_DELTA}KB ($(( RSS_DELTA / 1024 ))MB) | $([ "$RSS_DELTA" -lt 10240 ] && echo "✅" || echo "❌") |
| State restore after daemon restart | $TENANTS | $(grep daemon_restart "$METRICS_FILE" | python3 -c "import json,sys; print(json.loads(sys.stdin.readline()).get('restored',0))" 2>/dev/null || echo "N/A") | |
| Deploy mid-test | zero downtime | $(grep '"deploy"' "$METRICS_FILE" | python3 -c "import json,sys; d=json.loads(sys.stdin.readline()); print(f\"{d['deploy_ms']}ms\")" 2>/dev/null || echo "N/A") | |

## Spawn performance

- $TENANTS tenants spawned in ${SPAWN_SECS}s ($(( SPAWN_SECS * 1000 / TENANTS ))ms/tenant)

## Metrics

$(wc -l < "$METRICS_FILE") data points in $METRICS_FILE
EORESULTS

log "Results written to RESULTS.md"
log ""
log "=== SOAK TEST COMPLETE ==="
log "  Final: $FINAL_HEALTHY/$TENANTS healthy"
log "  creekd RSS: ${FINAL_RSS}KB (delta: ${RSS_DELTA}KB)"
log "  Restarts: $RESTART_TIMES"

# --- Teardown ---
log "Stopping creekd..."
kill "$(cat "$HERE/creekd.pid")" 2>/dev/null || true
rm -f "$HERE/creekd.pid"
