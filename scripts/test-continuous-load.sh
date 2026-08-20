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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Build the relay CLI once (exports CLI_BIN / CLI_BIN_DIR). This script has
# no cleanup() to clobber the EXIT trap, so the trap owns the mktemp dir.
echo "Building the CLI under test..."
. "$SCRIPT_DIR/lib/cli-build.sh"
build_relay_cli || exit 1
trap 'rm -rf "$CLI_BIN_DIR"' EXIT

get_redis_memory() {
    redis-cli INFO memory 2>/dev/null | grep "used_memory_human" | cut -d: -f2 | tr -d '\r' || echo "?"
}

# No CLI subcommand exposes these counts namespace-aware, so the key prefix
# is derived ONCE here. Localnet default namespace only — the counts read
# zero under a custom redis.namespace.base_prefix.
NS_BASE="ha"

report_state() {
    local active proved streams total_len key
    active=$(redis-cli --scan --pattern "${NS_BASE}:miner:sessions:*:state:active" 2>/dev/null | wc -l)
    proved=$(redis-cli --scan --pattern "${NS_BASE}:miner:sessions:*:state:proved" 2>/dev/null | wc -l)
    streams=0
    total_len=0
    for key in $(redis-cli --scan --pattern "${NS_BASE}:relays:*" 2>/dev/null); do
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
    out="$("$CLI_BIN" relay jsonrpc --localnet --service "$SERVICE_ID" \
        --relayer-url "$RELAYER_URL" \
        --load-test -n "$BATCH" --concurrency "$CONCURRENCY" --rps "$TARGET_RPS" \
        --all-suppliers 2>&1)"
    status=$?
    # Count only what the CLI reports as attempted: a failed run may have
    # sent anything from zero to a full batch, and assuming BATCH would
    # fabricate throughput while hot-spinning against a dead relayer.
    sent="$(printf '%s\n' "$out" | awk -F': *' '/^Total Requests:/ {print $2; exit}')"
    ok="$(printf '%s\n' "$out" | awk -F': *' '/^Successful:/ {print $2; exit}')"
    total_sent=$((total_sent + ${sent:-0}))
    total_ok=$((total_ok + ${ok:-0}))
    if [ "$status" -ne 0 ]; then
        echo "[$(date '+%H:%M:%S')] CLI batch failed (exit ${status}); last output lines:"
        printf '%s\n' "$out" | tail -5 | sed 's/^/    /'
        sleep 5
    else
        echo "[$(date '+%H:%M:%S')] batch=${sent:-0} verified_ok=${ok:-0} cumulative=${total_ok}/${total_sent}"
    fi
    report_state
done
