#!/bin/bash
#
# Continuous load test - sends relays at ~200 RPS until interrupted (Ctrl+C)
# Periodically reports success/failure rates and monitors for memory leaks
#
# Usage: ./scripts/test-continuous-load.sh
#        ./scripts/test-continuous-load.sh --rps 300
#

set -o pipefail

# Configuration
PATH_URL="${PATH_URL:-http://localhost:3069/v1}"
SERVICE_ID="${SERVICE_ID:-develop-http}"
TARGET_RPS="${TARGET_RPS:-200}"
REPORT_INTERVAL="${REPORT_INTERVAL:-30}"  # Print stats every N seconds
BATCH_SIZE="${BATCH_SIZE:-200}"           # Relays per hey invocation
HEY_CONCURRENCY="${HEY_CONCURRENCY:-50}"  # Concurrent requests per hey process

# Parse command line args
while [[ $# -gt 0 ]]; do
    case $1 in
        --rps)
            TARGET_RPS="$2"
            shift 2
            ;;
        --interval)
            REPORT_INTERVAL="$2"
            shift 2
            ;;
        *)
            echo "Unknown option: $1"
            echo "Usage: $0 [--rps N] [--interval N]"
            exit 1
            ;;
    esac
done

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# Relay request payload
PAYLOAD='{
  "jsonrpc": "2.0",
  "method": "eth_blockNumber",
  "params": [],
  "id": 1
}'

# Global counters
TOTAL_SENT=0
TOTAL_SUCCESS=0
TOTAL_ERRORS=0

# Temp directory
TEMP_DIR=$(mktemp -d)

cleanup() {
    echo ""
    echo -e "${YELLOW}Shutting down...${NC}"
    kill $(jobs -p) 2>/dev/null || true
    wait 2>/dev/null || true
    print_final_summary
    rm -rf "$TEMP_DIR"
    exit 0
}

trap cleanup EXIT INT TERM

# Helper functions
get_block_height() {
    curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"'
}

get_redis_memory() {
    redis-cli INFO memory 2>/dev/null | grep "used_memory_human" | cut -d: -f2 | tr -d '\r' || echo "?"
}

get_session_stats() {
    local active=$(redis-cli KEYS "ha:miner:sessions:*:state:active" 2>/dev/null | wc -l)
    local proved=$(redis-cli KEYS "ha:miner:sessions:*:state:proved" 2>/dev/null | wc -l)
    echo "${active}a/${proved}p"
}

get_stream_count() {
    local total=0
    for key in $(redis-cli KEYS "ha:relays:*" 2>/dev/null); do
        local len=$(redis-cli XLEN "$key" 2>/dev/null || echo 0)
        total=$((total + len))
    done
    echo "$total"
}

# Print banner
clear
echo -e "${BLUE}${BOLD}======================================================${NC}"
echo -e "${BLUE}${BOLD}  Continuous Load Test - Press Ctrl+C to stop${NC}"
echo -e "${BLUE}${BOLD}======================================================${NC}"
echo ""
echo -e "  URL:              ${GREEN}$PATH_URL${NC}"
echo -e "  Service:          ${GREEN}$SERVICE_ID${NC}"
echo -e "  Target RPS:       ${GREEN}$TARGET_RPS${NC}"
echo -e "  Report Interval:  ${GREEN}${REPORT_INTERVAL}s${NC}"
echo ""

# Check hey
if ! command -v hey &> /dev/null; then
    echo -e "${RED}ERROR: 'hey' not installed. Run: go install github.com/rakyll/hey@latest${NC}"
    exit 1
fi

# Pre-flight check
echo -e "${YELLOW}Testing PATH connectivity...${NC}"
TEST_RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Target-Service-Id: $SERVICE_ID" \
    -d "$PAYLOAD" \
    "$PATH_URL" 2>/dev/null || echo "000")

if [[ "$TEST_RESPONSE" != "200" ]]; then
    echo -e "${RED}ERROR: PATH not responding (HTTP $TEST_RESPONSE)${NC}"
    exit 1
fi
echo -e "${GREEN}PATH OK${NC}"
echo ""

# Record start
START_TIME=$(date +%s)
START_BLOCK=$(get_block_height)
START_MEMORY=$(get_redis_memory)
LAST_REPORT_TIME=$START_TIME

echo -e "Start: $(date '+%Y-%m-%d %H:%M:%S') | Block: $START_BLOCK | Memory: $START_MEMORY"
echo ""

print_final_summary() {
    local now=$(date +%s)
    local elapsed=$((now - START_TIME))
    local end_memory=$(get_redis_memory)

    [[ $elapsed -eq 0 ]] && elapsed=1
    local avg_rps=$((TOTAL_SENT / elapsed))
    local success_rate=0
    [[ $TOTAL_SENT -gt 0 ]] && success_rate=$(echo "scale=1; $TOTAL_SUCCESS * 100 / $TOTAL_SENT" | bc)

    echo ""
    echo -e "${BLUE}${BOLD}=== Final Summary ===${NC}"
    echo -e "  Duration:     ${elapsed}s"
    echo -e "  Total Sent:   $TOTAL_SENT"
    echo -e "  Successful:   ${GREEN}$TOTAL_SUCCESS${NC}"
    echo -e "  Errors:       ${RED}$TOTAL_ERRORS${NC}"
    echo -e "  Success Rate: ${success_rate}%"
    echo -e "  Avg RPS:      $avg_rps"
    echo -e "  Memory:       $START_MEMORY -> $end_memory"
}

# Print header
echo -e "${BLUE}─────────────────────────────────────────────────────────────────────────${NC}"
printf "${BOLD}%-9s %-10s %-10s %-8s %-7s %-6s %-10s %-12s %-8s${NC}\n" \
    "Time" "Sent" "Success" "Errors" "Rate" "RPS" "Memory" "Sessions" "Streams"
echo -e "${BLUE}─────────────────────────────────────────────────────────────────────────${NC}"

# Calculate concurrent batches needed for target RPS
# Each batch of 200 takes ~1s, so for 400 RPS we need 2 concurrent batches
CONCURRENT_BATCHES=$((TARGET_RPS / BATCH_SIZE))
[[ $CONCURRENT_BATCHES -lt 1 ]] && CONCURRENT_BATCHES=1
[[ $CONCURRENT_BATCHES -gt 10 ]] && CONCURRENT_BATCHES=10

echo "Running $CONCURRENT_BATCHES concurrent batches for ~${TARGET_RPS} RPS"
echo ""

# Function to run a batch and return results
run_batch() {
    local output_file="$TEMP_DIR/hey_$RANDOM.txt"
    hey -n "$BATCH_SIZE" \
        -c "$HEY_CONCURRENCY" \
        -m POST \
        -H "Content-Type: application/json" \
        -H "Target-Service-Id: $SERVICE_ID" \
        -d "$PAYLOAD" \
        "$PATH_URL" > "$output_file" 2>&1

    local success=$(grep -E "^\s*\[200\]" "$output_file" 2>/dev/null | awk '{print $2}' || echo "0")
    [[ -z "$success" ]] && success=0
    [[ ! "$success" =~ ^[0-9]+$ ]] && success=0
    echo "$success" > "$output_file.result"
    rm -f "$output_file"
}

# Main loop
while true; do
    # Run concurrent batches
    PIDS=()
    for ((i=0; i<CONCURRENT_BATCHES; i++)); do
        run_batch &
        PIDS+=($!)
    done

    # Wait for all batches
    for pid in "${PIDS[@]}"; do
        wait $pid 2>/dev/null || true
    done

    # Collect results
    BATCH_SUCCESS=0
    for result_file in "$TEMP_DIR"/*.result; do
        if [[ -f "$result_file" ]]; then
            success=$(cat "$result_file" 2>/dev/null || echo "0")
            [[ "$success" =~ ^[0-9]+$ ]] && BATCH_SUCCESS=$((BATCH_SUCCESS + success))
            rm -f "$result_file"
        fi
    done

    BATCH_SENT=$((BATCH_SIZE * CONCURRENT_BATCHES))
    BATCH_ERRORS=$((BATCH_SENT - BATCH_SUCCESS))
    [[ $BATCH_ERRORS -lt 0 ]] && BATCH_ERRORS=0

    # Update totals
    TOTAL_SENT=$((TOTAL_SENT + BATCH_SENT))
    TOTAL_SUCCESS=$((TOTAL_SUCCESS + BATCH_SUCCESS))
    TOTAL_ERRORS=$((TOTAL_ERRORS + BATCH_ERRORS))

    # Check if time to report
    NOW=$(date +%s)
    ELAPSED=$((NOW - START_TIME))
    TIME_SINCE_REPORT=$((NOW - LAST_REPORT_TIME))

    if [[ $TIME_SINCE_REPORT -ge $REPORT_INTERVAL ]]; then
        # Calculate stats
        [[ $ELAPSED -eq 0 ]] && ELAPSED=1
        RPS=$((TOTAL_SENT / ELAPSED))
        SUCCESS_RATE=0
        [[ $TOTAL_SENT -gt 0 ]] && SUCCESS_RATE=$(echo "scale=1; $TOTAL_SUCCESS * 100 / $TOTAL_SENT" | bc)

        MEMORY=$(get_redis_memory)
        SESSIONS=$(get_session_stats)
        STREAMS=$(get_stream_count)

        # Format time
        HOURS=$((ELAPSED / 3600))
        MINS=$(( (ELAPSED % 3600) / 60 ))
        SECS=$((ELAPSED % 60))
        TIME_FMT=$(printf "%02d:%02d:%02d" $HOURS $MINS $SECS)

        # Color code rate
        RATE_COLOR=$GREEN
        (( $(echo "$SUCCESS_RATE < 99" | bc -l) )) && RATE_COLOR=$YELLOW
        (( $(echo "$SUCCESS_RATE < 95" | bc -l) )) && RATE_COLOR=$RED

        printf "%-9s %-10s ${GREEN}%-10s${NC} ${RED}%-8s${NC} ${RATE_COLOR}%-7s${NC} %-6s %-10s %-12s %-8s\n" \
            "$TIME_FMT" "$TOTAL_SENT" "$TOTAL_SUCCESS" "$TOTAL_ERRORS" "${SUCCESS_RATE}%" "$RPS" "$MEMORY" "$SESSIONS" "$STREAMS"

        LAST_REPORT_TIME=$NOW
    fi
done
