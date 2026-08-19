#!/usr/bin/env bash
#
# Continuous load — sends SIGNED relays through the relay CLI at :8180 until
# interrupted, and reports Redis memory / session / stream growth on an
# interval so leaks show up as a trend, not a crash.
#
# This used to drive hey against the PATH gateway and count HTTP 200s — the
# exact signal PATH falsifies (it answers a relayer 503 with 200 and an empty
# body). Every relay below goes through `relay jsonrpc --load-test`, which
# ring-signs each request and verifies the supplier signature on each
# response; the success number is relays actually served, not statuses.
#
# Usage: ./scripts/test-continuous-load.sh [--rps N] [--interval N]
#   --rps       target relays per second (default 200)
#   --interval  seconds between reports (default 30)

set -o pipefail

SERVICE_ID="${SERVICE_ID:-develop-http}"
RELAYER_URL="${RELAYER_URL:-http://localhost:8180}"
TARGET_RPS="${TARGET_RPS:-200}"
REPORT_INTERVAL="${REPORT_INTERVAL:-30}"
CONCURRENCY="${CONCURRENCY:-20}"

while [[ $# -gt 0 ]]; do
    case $1 in
    --rps) TARGET_RPS="$2"; shift 2 ;;
    --interval) REPORT_INTERVAL="$2"; shift 2 ;;
    *)
        echo "Unknown option: $1"
        echo "Usage: $0 [--rps N] [--interval N]"
        exit 1
        ;;
    esac
done

cd "$(dirname "$0")/.." || exit 1

BIN_DIR="$(mktemp -d)"
trap 'rm -rf "$BIN_DIR"' EXIT
BIN="${BIN_DIR}/pocket-relay-miner"
echo "Building the CLI under test..."
go build -o "$BIN" . || exit 1

get_redis_memory() {
    redis-cli INFO memory 2>/dev/null | grep "used_memory_human" | cut -d: -f2 | tr -d '\r' || echo "?"
}

report_state() {
    local active proved streams total_len
    active=$(redis-cli KEYS "ha:miner:sessions:*:state:active" 2>/dev/null | wc -l)
    proved=$(redis-cli KEYS "ha:miner:sessions:*:state:proved" 2>/dev/null | wc -l)
    streams=0
    total_len=0
    for key in $(redis-cli KEYS "ha:relays:*" 2>/dev/null); do
        streams=$((streams + 1))
        total_len=$((total_len + $(redis-cli XLEN "$key" 2>/dev/null || echo 0)))
    done
    echo "  memory=$(get_redis_memory) sessions(active/proved)=${active}/${proved} streams=${streams} backlog=${total_len}"
}

# One CLI invocation sends REPORT_INTERVAL seconds worth of relays; the loop
# repeats until interrupted. --all-suppliers spreads the load so no single
# supplier hits the per-session economic cap.
BATCH=$((TARGET_RPS * REPORT_INTERVAL))
total_sent=0
total_ok=0
start_ts=$(date +%s)
START_MEMORY=$(get_redis_memory)

echo "Continuous load: ~${TARGET_RPS} RPS against ${RELAYER_URL} (service ${SERVICE_ID})"
echo "Start: $(date '+%Y-%m-%d %H:%M:%S') | Memory: ${START_MEMORY} | Ctrl+C to stop"
echo ""

finish() {
    local elapsed=$(( $(date +%s) - start_ts ))
    echo ""
    echo "=== Final report ==="
    echo "  elapsed=${elapsed}s sent=${total_sent} verified_ok=${total_ok}"
    echo "  memory: ${START_MEMORY} -> $(get_redis_memory)"
    report_state
    exit 0
}
trap finish INT TERM

while true; do
    out="$("$BIN" relay jsonrpc --localnet --service "$SERVICE_ID" \
        --relayer-url "$RELAYER_URL" \
        --load-test -n "$BATCH" --concurrency "$CONCURRENCY" --rps "$TARGET_RPS" \
        --all-suppliers 2>&1)"
    ok="$(printf '%s\n' "$out" | awk -F': *' '/^Successful:/ {print $2; exit}')"
    total_sent=$((total_sent + BATCH))
    total_ok=$((total_ok + ${ok:-0}))
    echo "[$(date '+%H:%M:%S')] batch=${BATCH} verified_ok=${ok:-0} cumulative=${total_ok}/${total_sent}"
    report_state
done
