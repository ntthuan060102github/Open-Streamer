#!/usr/bin/env bash
# Spawn N FFmpeg processes that push a 1080p test loop into open-streamer's
# RTMP server on 127.0.0.1:1935 under stream keys bench1..benchN.
#
# Pair this with scripts/create-streams.sh which POSTs N matching stream
# definitions whose primary input is rtmp://0.0.0.0:1935/live/bench{i}.
#
# Usage:
#   bench/scripts/source.sh 25                    # → spawn 25 sources
#   N=10 INPUT=bench/assets/clip.ts bench/scripts/source.sh
#
# Stop:
#   bench/scripts/source.sh stop                  # → kill every source
#
set -euo pipefail

BENCH_ROOT=$(cd "$(dirname "$0")"/.. && pwd)
RUNDIR=$BENCH_ROOT/.run
PIDFILE=$RUNDIR/source.pids
LOGDIR=$RUNDIR/source-logs

case ${1:-start} in
  stop)
    if [[ -f "$PIDFILE" ]]; then
      while read -r pid; do
        [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
      done <"$PIDFILE"
      rm -f "$PIDFILE"
      echo "[source] stopped"
    fi
    exit 0
    ;;
  start|[0-9]*)
    [[ "${1:-start}" != "start" ]] && N=$1
    ;;
esac

N=${N:-10}
INPUT=${INPUT:-$BENCH_ROOT/assets/sample-1080p.ts}
SERVER=${SERVER:-rtmp://127.0.0.1:1935/live}
PREFIX=${PREFIX:-bench}

if [[ ! -f "$INPUT" ]]; then
  echo "[source] missing input file: $INPUT"
  echo "[source] generate it with:  bench/scripts/prepare.sh sample"
  exit 1
fi

mkdir -p "$LOGDIR"
: >"$PIDFILE"

echo "[source] spawning $N RTMP publishers → $SERVER/$PREFIX{1..$N}"
for i in $(seq 1 "$N"); do
  # -nostdin avoids ffmpeg getting SIGTTIN/SIGTTOU from terminal job control
  # when this script runs in an interactive shell or tmux pane.
  ffmpeg -hide_banner -loglevel error -nostdin \
    -re -stream_loop -1 -i "$INPUT" \
    -c copy -f flv "$SERVER/${PREFIX}${i}" \
    </dev/null >"$LOGDIR/src${i}.log" 2>&1 &
  echo $! >>"$PIDFILE"
done

echo "[source] $(wc -l <"$PIDFILE") workers running. PIDs in $PIDFILE"
echo "[source] tail logs: tail -f $LOGDIR/src1.log"
echo "[source] stop:      bench/scripts/source.sh stop"
