#!/bin/bash
# cgroup memory.max experiment — paired with the memory.high experiment
# (run.sh). Three phases:
#   A. paired containment: memory.high=256M + memory.max ∈ {384, 512,
#      768, 1024} MB. Normal 100 MiB/s leaker. Expect oom_kills=0
#      everywhere — memory.high should catch first.
#   B. defeat memory.high alone: try faster / bigger allocation
#      patterns. Goal: find a workload that crosses the soft cap
#      before throttle can react, so memory.max actually has to fire.
#   C. multi-leaker storm: 4 leakers simultaneously, all paired.
#      host MemAvailable trajectory + per-cg oom_kill count.
#
# Linux + cgroup v2 + root. Writes /tmp/cgroup-max-exp.log.
set -uo pipefail

EXP_SLICE=/sys/fs/cgroup/creek-exp
LOG=/tmp/cgroup-max-exp.log
> "$LOG"
log() { printf "%s\n" "$*" | tee -a "$LOG"; }
mb()  { echo $(( $1 / 1024 / 1024 )); }

if [ ! -d /sys/fs/cgroup ] || [ ! -f /sys/fs/cgroup/cgroup.controllers ]; then
    echo "needs Linux cgroup v2"; exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
    echo "needs root for cgroup writes — re-run with sudo"; exit 2
fi

mkdir -p "$EXP_SLICE"
echo "+memory +pids +cpu" > "$EXP_SLICE/cgroup.subtree_control" 2>/dev/null || true

# Three leaker variants — written once so phase B can swap them in.

# v1: steady 100 MiB/s, the same pattern memory.high handled cleanly.
cat > /tmp/leaker_steady.js <<'EOF'
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

# v2: single 768 MiB allocation + fill in one shot. Tests whether
# memory.high throttle can react before the page-fault loop touches
# every page.
cat > /tmp/leaker_singleshot.js <<'EOF'
const SIZE = 768 * 1024 * 1024;
const buf = new Uint8Array(SIZE);
buf.fill(42);
// hold for 25 s so observer can read peak
const start = Date.now();
setInterval(() => {
  if (Date.now() - start > 25000) process.exit(0);
}, 500);
EOF

# v3: aggressive 1 GiB/s — 10× faster than steady. Tests whether the
# throttle is fast enough at higher allocation rates.
cat > /tmp/leaker_fast.js <<'EOF'
const a = [];
let i = 0;
const start = Date.now();
const id = setInterval(() => {
  // 10 × 10 MiB per tick × 10 ticks/s = 1 GiB/s
  for (let k = 0; k < 10; k++) {
    const buf = new Uint8Array(10 * 1024 * 1024);
    buf.fill((i * 10 + k) % 256);
    a.push(buf);
  }
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

read_high_count() { awk '/^high /{print $2; exit}'     "$1/memory.events" 2>/dev/null || echo 0; }
read_oom_count()  { awk '/^oom_kill /{print $2; exit}' "$1/memory.events" 2>/dev/null || echo 0; }
read_max_count()  { awk '/^max /{print $2; exit}'      "$1/memory.events" 2>/dev/null || echo 0; }
read_mem_current(){ cat "$1/memory.current" 2>/dev/null || echo 0; }
host_avail_mb()   { awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo; }

# Run one leaker inside a paired-cap cgroup and report timeline.
# args: cg_name leaker_script high_bytes max_bytes [timeout_sec=60]
# Returns: elapsed peak_mb high_total max_total oom_total avail_mb hung
# hung=1 means the leaker stopped responding (event loop frozen by
# reclaim pressure) and we had to SIGKILL after timeout.
run_paired() {
    local name="$1" script="$2" high="$3" max="$4" timeout="${5:-60}"
    local cg="$EXP_SLICE/$name"
    rmdir "$cg" 2>/dev/null || true
    mkdir -p "$cg"
    echo "$high" > "$cg/memory.high"
    echo "$max"  > "$cg/memory.max"

    local pid
    pid=$(spawn_into_cgroup "$cg" bun "$script")

    local start=$(date +%s)
    local peak=0 hung=0
    while kill -0 "$pid" 2>/dev/null; do
        if [ $(( $(date +%s) - start )) -ge "$timeout" ]; then
            hung=1
            kill -9 "$pid" 2>/dev/null
            # drain any sibling pids in the cgroup
            for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
            break
        fi
        local cur
        cur=$(read_mem_current "$cg")
        [ "$cur" -gt "$peak" ] && peak=$cur
        local oom
        oom=$(read_oom_count "$cg")
        if [ "$oom" -gt 0 ]; then break; fi
        sleep 0.5
    done
    sleep 0.5
    local peak_after_exit
    peak_after_exit=$(read_mem_current "$cg")
    [ "$peak_after_exit" -gt "$peak" ] && peak=$peak_after_exit

    local elapsed=$(( $(date +%s) - start ))
    local high_total max_total oom_total avail_now
    high_total=$(read_high_count "$cg")
    max_total=$(read_max_count "$cg")
    oom_total=$(read_oom_count "$cg")
    avail_now=$(host_avail_mb)

    echo "$elapsed $(mb $peak) $high_total $max_total $oom_total $avail_now $hung"

    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    sleep 1
    rmdir "$cg" 2>/dev/null || true
}

# ===========================================================
# PHASE A — paired containment under steady leaker
# ===========================================================
log ""
log "================================================================"
log "PHASE A — paired memory.high=256M + memory.max varied, steady leaker"
log "================================================================"
log ""
log "steady leaker = 100 MiB/s for 30 s; memory.high handled this alone"
log "at +11% overshoot. Expect memory.max to never fire here."
log "60 s wall-clock timeout per cap — leakers that hang in throttle"
log "loop are force-killed; 'hung?' column flags them."
log ""
log "case    | wall_s | peak_mb | high_evt | max_evt | oom_kills | hung? | avail_mb"
log "--------+--------+---------+----------+---------+-----------+-------+---------"

HIGH=$((256 * 1024 * 1024))
report_row() {
    local label="$1"; shift
    local r="$*"
    local elapsed peak htot mtot otot avail hung
    elapsed=$(echo $r | awk '{print $1}')
    peak=$(echo $r    | awk '{print $2}')
    htot=$(echo $r    | awk '{print $3}')
    mtot=$(echo $r    | awk '{print $4}')
    otot=$(echo $r    | awk '{print $5}')
    avail=$(echo $r   | awk '{print $6}')
    hung=$(echo $r    | awk '{print $7}')
    printf "%-7s | %6d | %5d MB | %8d | %7d | %9d | %5d | %7d\n" \
        "$label" "$elapsed" "$peak" "$htot" "$mtot" "$otot" "$hung" "$avail" | tee -a "$LOG"
}

# Baseline confirm: 384M, 768M, 1024M one trial each.
for MAX_MB in 384 768 1024; do
    MAX=$(( MAX_MB * 1024 * 1024 ))
    report_row "${MAX_MB}M" "$(run_paired "phaseA-${MAX_MB}M" /tmp/leaker_steady.js "$HIGH" "$MAX" 60)"
done

# 512M reproducibility check — was the anomalous case in the v1 run
# (1529 s wall time, 38586 throttle events, same peak as others).
# Three trials to see if the hang is reproducible.
log ""
log "  -- 512M reproducibility trials (this case hung in v1 run) --"
for trial in 1 2 3; do
    MAX=$(( 512 * 1024 * 1024 ))
    report_row "512M.$trial" "$(run_paired "phaseA-512M-t$trial" /tmp/leaker_steady.js "$HIGH" "$MAX" 60)"
done

# ===========================================================
# PHASE B — single-shot 768 MiB allocation (bypass throttle?)
#          + fast 1 GiB/s allocation
# ===========================================================
log ""
log "================================================================"
log "PHASE B — patterns designed to defeat memory.high alone"
log "================================================================"
log ""
log "Pair high=256M + max=1024M for both variants. If max_evt > 0 the"
log "soft cap was overrun; if oom_kills > 0 the hard cap actually fired."
log "60 s timeout per case."
log ""
log "variant     | wall_s | peak_mb | high_evt | max_evt | oom_kills | hung? | avail_mb"
log "------------+--------+---------+----------+---------+-----------+-------+---------"

MAX=$(( 1024 * 1024 * 1024 ))

for variant in singleshot fast; do
    case "$variant" in
        singleshot) SCRIPT=/tmp/leaker_singleshot.js ;;
        fast)       SCRIPT=/tmp/leaker_fast.js ;;
    esac
    report_row "$variant" "$(run_paired "phaseB-${variant}" "$SCRIPT" "$HIGH" "$MAX" 60)"
done

# ===========================================================
# PHASE C — multi-leaker storm
# ===========================================================
log ""
log "================================================================"
log "PHASE C — 4 simultaneous steady leakers, each paired high=256M max=1G"
log "================================================================"
log ""
log "Watch host MemAvailable + per-cg oom_kills. If host stays alive"
log "and per-cg counters stay 0, paired caps hold under storm."
log ""

declare -A STORM_PIDS=()
STORM_START_AVAIL=$(host_avail_mb)
log "host MemAvailable at start: ${STORM_START_AVAIL} MB"
for i in 1 2 3 4; do
    cg="$EXP_SLICE/phaseC-$i"
    rmdir "$cg" 2>/dev/null || true
    mkdir -p "$cg"
    echo "$HIGH" > "$cg/memory.high"
    echo "$MAX"  > "$cg/memory.max"
    pid=$(spawn_into_cgroup "$cg" bun /tmp/leaker_steady.js)
    STORM_PIDS[$i]=$pid
done

# Watch host MemAvailable every 2 s; record minimum. 45 s wall — long
# enough for steady leakers to either complete or hang.
storm_min=$STORM_START_AVAIL
storm_end=$(( $(date +%s) + 45 ))
while [ $(date +%s) -lt $storm_end ]; do
    a=$(host_avail_mb)
    [ "$a" -lt "$storm_min" ] && storm_min=$a
    sleep 2
done

# Force-kill any leakers that haven't exited (they're in the hung state).
for i in 1 2 3 4; do
    cg="$EXP_SLICE/phaseC-$i"
    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
done
sleep 1

log ""
log "host MemAvailable min during storm: ${storm_min} MB"
log "delta from start: $(( STORM_START_AVAIL - storm_min )) MB"
log ""
log "per-cg final state:"
log "  cg | peak_mb | high_evt | max_evt | oom_kills"
log "  ---+---------+----------+---------+----------"
for i in 1 2 3 4; do
    cg="$EXP_SLICE/phaseC-$i"
    peak=$(read_mem_current "$cg")
    h=$(read_high_count "$cg"); m=$(read_max_count "$cg"); o=$(read_oom_count "$cg")
    printf "  %d  | %5d MB | %8d | %7d | %9d\n" "$i" "$(mb $peak)" "$h" "$m" "$o" | tee -a "$LOG"
    for p in $(cat "$cg/cgroup.procs" 2>/dev/null); do kill -9 "$p" 2>/dev/null; done
    sleep 0.3
    rmdir "$cg" 2>/dev/null || true
done

log ""
log "================================================================"
log "DONE — full log: $LOG"
log "================================================================"
