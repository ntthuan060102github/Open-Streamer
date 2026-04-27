#!/usr/bin/env bash
# Orchestrate one full benchmark run:
#   1. create N streams from <profile> payload
#   2. spawn N RTMP source publishers
#   3. wait warm-up
#   4. capture sample.csv during steady window
#   5. snapshot runtime + streams JSON
#   6. tear down
#
# Output goes to results/<run-id>/.
#
# Usage:
#   bench/scripts/run-bench.sh <run-id> <N> <profile> [steady_sec] [warmup_sec]
#
# Examples:
#   bench/scripts/run-bench.sh A2  10 passthrough
#   bench/scripts/run-bench.sh B3  4  abr3-legacy 300 60
#   bench/scripts/run-bench.sh C3  4  abr3-multi
#
set -euo pipefail

[[ $# -lt 3 ]] && { echo "Usage: $0 <run-id> <N> <profile> [steady_sec=300] [warmup_sec=60]"; exit 1; }

RUN_ID=$1
N=$2
PROFILE=$3
STEADY=${4:-300}
WARMUP=${5:-60}

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
SCRIPTS=$BENCH_ROOT/scripts
OUTDIR=$BENCH_ROOT/results/$RUN_ID
API=${API:-http://127.0.0.1:8080}

mkdir -p "$OUTDIR"
echo "[run-bench] === $RUN_ID  N=$N  profile=$PROFILE  warmup=${WARMUP}s  steady=${STEADY}s ==="
echo "[run-bench] output → $OUTDIR"

trap '"$SCRIPTS"/source.sh stop >/dev/null 2>&1 || true; "$SCRIPTS"/create-streams.sh delete "$N" >/dev/null 2>&1 || true' EXIT

# Snapshot config + state at t0. /streams response embeds per-stream runtime in
# the .runtime field — there is no separate /runtime endpoint.
curl -fs "$API/config"  > "$OUTDIR/config.json"   || echo "[run-bench] WARN: cannot fetch /config"
curl -fs "$API/streams" > "$OUTDIR/runtime-t0.json" || true

"$SCRIPTS"/create-streams.sh "$N" "$PROFILE" | tee "$OUTDIR/create.log"
"$SCRIPTS"/source.sh "$N" | tee -a "$OUTDIR/create.log"

echo "[run-bench] warm-up ${WARMUP}s..."
sleep "$WARMUP"

curl -fs "$API/streams" > "$OUTDIR/runtime-warmup.json" || true

echo "[run-bench] sampling for ${STEADY}s..."
"$SCRIPTS"/sample.sh "$STEADY" "$OUTDIR/sample.csv" &
SAMPLE_PID=$!

# Mid-window snapshot
sleep $((STEADY / 2))
curl -fs "$API/streams" > "$OUTDIR/runtime-mid.json" || true
curl -fs "$API/streams" > "$OUTDIR/streams-mid.json" || true

wait "$SAMPLE_PID"

curl -fs "$API/streams" > "$OUTDIR/runtime-end.json" || true
curl -fs "$API/streams" > "$OUTDIR/streams-end.json" || true
curl -fs "$API/metrics" > "$OUTDIR/metrics-end.txt" 2>/dev/null || true

echo "[run-bench] === done. files in $OUTDIR ==="
ls -la "$OUTDIR"
