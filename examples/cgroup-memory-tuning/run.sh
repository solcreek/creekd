#!/bin/bash
# cgroup memory.high experiment — three phases:
#   1. false-positive sweep: 4 stacks at memory.high=256M, confirm no
#      throttle under sustained 30 rps × 60 s.
#   2. containment: deliberate leaker at 128M / 256M / 512M, measure
#      time-to-first-throttle, peak overshoot, throttle event count.
#   3. sibling impact: 4 normal Hono apps + 1 leaker (contained at
#      256M); latency on a normal app baseline vs under-leaker.
#
# Linux + cgroup v2 + root required (writes /sys/fs/cgroup/...).
#
# Results land in /tmp/cgroup-exp.log alongside stdout.
set -uo pipefail

FIXTURES="$(cd "$(dirname "$0")/../stack-density/stacks" && pwd)"
EXP_SLICE=/sys/fs/cgroup/creek-exp
LOG=/tmp/cgroup-exp.log
> "$LOG"
log() { printf "%s\n" "$*" | tee -a "$LOG"; }
mb()  { echo $(( $1 / 1024 / 1024 )); }

if [ ! -d /sys/fs/cgroup ] || [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
    echo "needs Linux cgroup v2 (no /sys/fs/cgroup/cgroup.controllers found)"
    exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
    echo "needs root for cgroup writes — re-run with sudo"
    exit 2
fi

mkdir -p "$EXP_SLICE"
echo "+memory +pids +cpu" > "$EXP_SLICE/cgroup.subtree_control" 2>/dev/null || true

# Leaker on disk so we don't fight bash quoting. .fill() is critical
# — without it, Uint8Array is virtual-only and the kernel never backs
# the pages with physical memory, so memory.high never triggers.
cat > /tmp/leaker.js <<'EOF'
const a = [];
let i = 0;
const start = Date.now();
const id = setInterval(() => {
  const buf = new Uint8Array(10 * 1024 * 1024);
  buf.fill(i % 256);
  a.push(buf);
  i++;
  if (Date.now() - start > 30000) { clearInterval(id); process.exit(0); }
}, 100);
EOF

cleanup() {
    for d in "$EXP_SLICE"/*/; do
        [ -d "$d" ] || continue
        for pid in $(cat "$d/cgroup.procs" 2>/dev/null); do kill -9 "$pid" 2>/dev/null || true; done
        rmdir "$d" 2>/dev/null || true
    done
}
trap cleanup EXIT

spawn_into_cgroup() {
    local cg="$1"; shift
    setsid bash -c "echo \$\$ > '$cg/cgroup.procs'; exec \"\$@\"" -- "$@" >/dev/null 2>&1 &
    echo $!
}

read_high_count() { awk '/^high /{print $2; exit}' "$1/memory.events" 2>/dev/null || echo 0; }
read_oom_count()  { awk '/^oom_kill /{print $2; exit}' "$1/memory.events" 2>/dev/null || echo 0; }
read_mem_current(){ cat "$1/memory.current" 2>/dev/null || echo 0; }

wait_reachable() {
    local port="$1" deadline=$(( $(date +%s) + 30 ))
    while [ $(date +%s) -lt $deadline ]; do
        curl -sf "http://127.0.0.1:$port/" -m 1 -o /dev/null 2>/dev/null && return 0
        sleep 0.5
    done
    return 1
}

do_sustained() {
    local port="$1" dur="$2" rps="$3" tmp=$(mktemp) count=0
    local interval_ms=$(( 1000 / rps ))
    local end=$(( $(date +%s) + dur ))
    while [ $(date +%s) -lt $end ]; do
        local t0=$(date +%s%N)
        if curl -sf "http://127.0.0.1:$port/" -m 2 -o /dev/null 2>/dev/null; then
            local t1=$(date +%s%N)
            echo $(( (t1 - t0) / 1000000 )) >> "$tmp"
            count=$((count+1))
        fi
        sleep "0.$(printf '%03d' $interval_ms)"
    done
    local p50=$(sort -n "$tmp" | awk -v c="$count" 'NR==int(c*0.5){print; exit}')
    local p99=$(sort -n "$tmp" | awk -v c="$count" 'NR==int(c*0.99){print; exit}')
    rm -f "$tmp"
    echo "$count ${p50:-0} ${p99:-0}"
}

# ===========================================================
# PHASE 1 — false-positive sweep at memory.high = 256M
# ===========================================================
log ""
log "================================================================"
log "PHASE 1 — false-positive sweep, memory.high = 256M (the candidate)"
log "================================================================"
log ""
log "stack      | throttle_events | peak_mem | sustained 30rps×60s (req# / p50 / p99 ms)"
log "-----------+-----------------+----------+-------------------------------------------"

LIMIT_BYTES=$((256 * 1024 * 1024))
declare -A STACK_ENTRY=(
    [bun-hello]="$FIXTURES/bun-hello/server.js"
    [hono]="$FIXTURES/hono/server.js"
    [sveltekit]="$FIXTURES/sveltekit/build/index.js"
    [astro]="$FIXTURES/astro/dist/server/entry.mjs"
)

PORT=22000
for stack in bun-hello hono sveltekit astro; do
    cg="$EXP_SLICE/sweep-$stack"
    rmdir "$cg" 2>/dev/null || true
    mkdir -p "$cg"
    echo "$LIMIT_BYTES" > "$cg/memory.high"

    pid=$(spawn_into_cgroup "$cg" env PORT=$PORT HOST=127.0.0.1 bun run "${STACK_ENTRY[$stack]}")
    if ! wait_reachable "$PORT"; then
        log "  $stack: TIMEOUT"; kill -9 "$pid" 2>/dev/null; PORT=$((PORT+1)); continue
    fi
    sleep 3

    high_before=$(read_high_count "$cg")
    result=$(do_sustained "$PORT" 60 30)
    high_after=$(read_high_count "$cg")
    peak_mb=$(mb $(read_mem_current "$cg"))
    delta=$((high_after - high_before))

    printf "%-10s | %15d | %5d MB | %s\n" \
        "$stack" "$delta" "$peak_mb" "$result" | tee -a "$LOG"

    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    sleep 1
    rmdir "$cg" 2>/dev/null || true
    PORT=$((PORT+1))
done

# ===========================================================
# PHASE 2 — containment behaviour
# ===========================================================
log ""
log "================================================================"
log "PHASE 2 — leaker containment at memory.high = 128M / 256M / 512M"
log "================================================================"
log ""
log "leaker = bun /tmp/leaker.js (allocates + fills 10 MiB / 100 ms for 30 s)"
log "no memory.max set — only memory.high. Throttle, no OOM."
log ""
log "limit | t_first_throttle | peak_mem | total_high | oom_kills | finished?"
log "------+------------------+----------+------------+-----------+----------"

for LIMIT in 128M 256M 512M; do
    case "$LIMIT" in
        128M) LIMIT_BYTES=$((128*1024*1024));;
        256M) LIMIT_BYTES=$((256*1024*1024));;
        512M) LIMIT_BYTES=$((512*1024*1024));;
    esac
    cg="$EXP_SLICE/leak-$LIMIT"
    rmdir "$cg" 2>/dev/null || true
    mkdir -p "$cg"
    echo "$LIMIT_BYTES" > "$cg/memory.high"

    leaker_pid=$(spawn_into_cgroup "$cg" bun /tmp/leaker.js)

    start=$(date +%s)
    t_first=0
    peak=0
    while kill -0 "$leaker_pid" 2>/dev/null; do
        high_now=$(read_high_count "$cg")
        cur=$(read_mem_current "$cg")
        [ "$cur" -gt "$peak" ] && peak=$cur
        if [ "$high_now" -gt 0 ] && [ "$t_first" -eq 0 ]; then
            t_first=$(( $(date +%s) - start ))
        fi
        sleep 1
    done
    elapsed=$(( $(date +%s) - start ))
    high_total=$(read_high_count "$cg")
    oom_total=$(read_oom_count "$cg")
    if [ "$elapsed" -ge 28 ]; then status="finished cleanly"; else status="exited early (${elapsed}s)"; fi
    printf "%-5s | %16d | %5d MB | %10d | %9d | %s\n" \
        "$LIMIT" "$t_first" "$(mb $peak)" "$high_total" "$oom_total" "$status" | tee -a "$LOG"

    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    sleep 1
    rmdir "$cg" 2>/dev/null || true
done

# ===========================================================
# PHASE 3 — sibling impact
# ===========================================================
log ""
log "================================================================"
log "PHASE 3 — sibling impact: 4 normal Hono + 1 contained leaker (256M)"
log "================================================================"

NORMAL_PORTS=(23001 23002 23003 23004)
declare -A NORMAL_PIDS=()
for port in "${NORMAL_PORTS[@]}"; do
    cg="$EXP_SLICE/normal-$port"
    rmdir "$cg" 2>/dev/null || true
    mkdir -p "$cg"
    pid=$(spawn_into_cgroup "$cg" env PORT=$port HOST=127.0.0.1 bun run "${STACK_ENTRY[hono]}")
    NORMAL_PIDS[$port]=$pid
done
for port in "${NORMAL_PORTS[@]}"; do wait_reachable "$port" || true; done
sleep 3

log ""
log "  baseline (no leaker), 50rps × 15s on port 23001:"
base=$(do_sustained 23001 15 50)
log "    req=$(echo $base | awk '{print $1}')  p50=$(echo $base | awk '{print $2}')ms  p99=$(echo $base | awk '{print $3}')ms"

LEAK_CG="$EXP_SLICE/leak-impact"
rmdir "$LEAK_CG" 2>/dev/null || true
mkdir -p "$LEAK_CG"
echo $((256*1024*1024)) > "$LEAK_CG/memory.high"
leak_pid=$(spawn_into_cgroup "$LEAK_CG" bun /tmp/leaker.js)
sleep 10

log ""
log "  under contained leaker (high=256M), 50rps × 15s on port 23001:"
under=$(do_sustained 23001 15 50)
log "    req=$(echo $under | awk '{print $1}')  p50=$(echo $under | awk '{print $2}')ms  p99=$(echo $under | awk '{print $3}')ms"

high_in=$(read_high_count "$LEAK_CG")
oom_in=$(read_oom_count "$LEAK_CG")
leak_peak=$(mb $(read_mem_current "$LEAK_CG"))
log ""
log "  leaker state: memory.current=${leak_peak}MB, high_events=$high_in, oom_kills=$oom_in"
log "  host MemAvailable: $(awk '/MemAvailable/{print int($2/1024)" MB"}' /proc/meminfo)"

kill -9 "$leak_pid" 2>/dev/null
for p in $(cat "$LEAK_CG/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
rmdir "$LEAK_CG" 2>/dev/null || true
for port in "${NORMAL_PORTS[@]}"; do
    kill -9 "${NORMAL_PIDS[$port]}" 2>/dev/null
    cg="$EXP_SLICE/normal-$port"
    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    rmdir "$cg" 2>/dev/null || true
done

log ""
log "================================================================"
log "DONE — full log: $LOG"
log "================================================================"
