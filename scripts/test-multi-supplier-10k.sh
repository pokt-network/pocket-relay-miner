#!/bin/bash
#
# Send 10,000 relays to PATH across ALL suppliers (no supplier pinning)
# Uses parallel batches for maximum throughput
#
# This tests multi-supplier claim/proof submission to verify:
# 1. No blocking between claim groups
# 2. Worker pool properly handles multiple suppliers
# 3. All suppliers can claim/prove within windows
#

set -euo pipefail

# Configuration
PATH_URL="${PATH_URL:-http://localhost:3069/v1}"
SERVICE_ID="${SERVICE_ID:-develop-http}"
TOTAL_RELAYS="${TOTAL_RELAYS:-10000}"
BATCH_SIZE="${BATCH_SIZE:-500}"        # Relays per batch
CONCURRENT_BATCHES="${CONCURRENT_BATCHES:-10}"  # Parallel hey processes
HEY_CONCURRENCY="${HEY_CONCURRENCY:-50}"  # Concurrent requests per hey process

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Relay request payload (simple JSON-RPC request)
PAYLOAD='{
  "jsonrpc": "2.0",
  "method": "eth_blockNumber",
  "params": [],
  "id": 1
}'

# Print banner
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Multi-Supplier Relay Test (10k)${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo -e "URL:               ${GREEN}$PATH_URL${NC}"
echo -e "Service:           ${GREEN}$SERVICE_ID${NC}"
echo -e "Total Relays:      ${GREEN}$TOTAL_RELAYS${NC}"
echo -e "Batch Size:        ${GREEN}$BATCH_SIZE${NC}"
echo -e "Concurrent Batches: ${GREEN}$CONCURRENT_BATCHES${NC}"
echo -e "Hey Concurrency:   ${GREEN}$HEY_CONCURRENCY${NC}"
echo ""
echo -e "${YELLOW}NOTE: No Target-Suppliers header - relays distributed across all suppliers${NC}"
echo ""

# Check if hey is installed
if ! command -v hey &> /dev/null; then
    echo -e "${RED}ERROR: 'hey' is not installed. Install with: go install github.com/rakyll/hey@latest${NC}"
    exit 1
fi

# Get current block height
get_block_height() {
    curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "unknown"'
}

# Get miner logs for specific patterns
check_miner_logs() {
    local pattern="$1"
    kubectl logs -l app=miner --tail=50 2>/dev/null | grep -E "$pattern" || true
}

# Record start time and block
START_TIME=$(date +%s)
START_BLOCK=$(get_block_height)
echo -e "Start Time:        ${GREEN}$(date '+%Y-%m-%d %H:%M:%S')${NC}"
echo -e "Start Block:       ${GREEN}$START_BLOCK${NC}"
echo ""

# Pre-flight check: Verify PATH is responding
echo -e "${YELLOW}Pre-flight check: Testing PATH connectivity...${NC}"
TEST_RESPONSE=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Target-Service-Id: $SERVICE_ID" \
    -d "$PAYLOAD" \
    "$PATH_URL" 2>/dev/null || echo "000")

if [[ "$TEST_RESPONSE" != "200" ]]; then
    echo -e "${RED}ERROR: PATH is not responding (HTTP $TEST_RESPONSE)${NC}"
    echo "Make sure tilt is running: tilt up"
    exit 1
fi
echo -e "${GREEN}PATH is responding (HTTP 200)${NC}"
echo ""

# Calculate number of batches
NUM_BATCHES=$((TOTAL_RELAYS / BATCH_SIZE))
REMAINDER=$((TOTAL_RELAYS % BATCH_SIZE))
if [[ $REMAINDER -gt 0 ]]; then
    NUM_BATCHES=$((NUM_BATCHES + 1))
fi

echo -e "${BLUE}Sending $TOTAL_RELAYS relays in $NUM_BATCHES batches (${BATCH_SIZE}/batch)...${NC}"
echo ""

# Create temp directory for batch results
TEMP_DIR=$(mktemp -d)
trap "rm -rf $TEMP_DIR" EXIT

# Function to run a batch
run_batch() {
    local batch_num=$1
    local count=$2
    local output_file="$TEMP_DIR/batch_${batch_num}.txt"

    # NOTE: No Target-Suppliers header - let PATH distribute across suppliers
    hey -n "$count" \
        -c "$HEY_CONCURRENCY" \
        -m POST \
        -H "Content-Type: application/json" \
        -H "Target-Service-Id: $SERVICE_ID" \
        -d "$PAYLOAD" \
        "$PATH_URL" > "$output_file" 2>&1

    echo "Batch $batch_num complete ($count relays)"
}

# Export for parallel execution
export -f run_batch
export PATH_URL SERVICE_ID PAYLOAD TEMP_DIR HEY_CONCURRENCY

# Track progress
TOTAL_SENT=0
BATCH_NUM=0

# Run batches in parallel
echo -e "${YELLOW}Starting parallel relay submission...${NC}"
echo ""

while [[ $TOTAL_SENT -lt $TOTAL_RELAYS ]]; do
    # Calculate how many batches to run in this round
    BATCHES_THIS_ROUND=0
    PIDS=()

    for ((i=0; i<CONCURRENT_BATCHES && TOTAL_SENT<TOTAL_RELAYS; i++)); do
        BATCH_NUM=$((BATCH_NUM + 1))

        # Calculate batch size (handle remainder)
        REMAINING=$((TOTAL_RELAYS - TOTAL_SENT))
        if [[ $REMAINING -lt $BATCH_SIZE ]]; then
            THIS_BATCH=$REMAINING
        else
            THIS_BATCH=$BATCH_SIZE
        fi

        # Run batch in background
        run_batch $BATCH_NUM $THIS_BATCH &
        PIDS+=($!)

        TOTAL_SENT=$((TOTAL_SENT + THIS_BATCH))
        BATCHES_THIS_ROUND=$((BATCHES_THIS_ROUND + 1))
    done

    # Wait for this round of batches to complete
    for pid in "${PIDS[@]}"; do
        wait $pid || true
    done

    # Progress update
    CURRENT_BLOCK=$(get_block_height)
    ELAPSED=$(($(date +%s) - START_TIME))
    RPS=$((TOTAL_SENT / (ELAPSED + 1)))

    echo -e "Progress: ${GREEN}$TOTAL_SENT${NC}/${TOTAL_RELAYS} relays sent | Block: ${GREEN}$CURRENT_BLOCK${NC} | ${GREEN}${RPS} RPS${NC}"
done

echo ""
echo -e "${GREEN}All relays submitted!${NC}"
echo ""

# Summary statistics
END_TIME=$(date +%s)
END_BLOCK=$(get_block_height)
TOTAL_TIME=$((END_TIME - START_TIME))
BLOCKS_ELAPSED=$((END_BLOCK - START_BLOCK))
OVERALL_RPS=$((TOTAL_RELAYS / (TOTAL_TIME + 1)))

echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Summary${NC}"
echo -e "${BLUE}========================================${NC}"
echo -e "End Time:          ${GREEN}$(date '+%Y-%m-%d %H:%M:%S')${NC}"
echo -e "End Block:         ${GREEN}$END_BLOCK${NC}"
echo -e "Total Time:        ${GREEN}${TOTAL_TIME}s${NC}"
echo -e "Blocks Elapsed:    ${GREEN}$BLOCKS_ELAPSED${NC}"
echo -e "Overall RPS:       ${GREEN}$OVERALL_RPS${NC}"
echo ""

# Aggregate hey results
echo -e "${BLUE}Aggregated Results:${NC}"
echo ""

TOTAL_REQUESTS=0
TOTAL_SUCCESSES=0
TOTAL_ERRORS=0
TOTAL_LATENCY=0
BATCH_COUNT=0

for result_file in "$TEMP_DIR"/batch_*.txt; do
    if [[ -f "$result_file" ]]; then
        BATCH_COUNT=$((BATCH_COUNT + 1))

        # Extract stats from hey output
        REQUESTS=$(grep -oP '\[\d+\]\s+\K\d+' "$result_file" | head -1 || echo "0")
        SUCCESSES=$(grep -oP 'Status code distribution:.*200\s+\K\d+' "$result_file" || echo "0")

        # Show individual batch summary
        if [[ -n "$REQUESTS" && "$REQUESTS" != "0" ]]; then
            # Extract p99 latency
            P99=$(grep -oP '99%.*\K[\d.]+' "$result_file" | head -1 || echo "N/A")
            echo "  Batch $BATCH_COUNT: requests=$REQUESTS, p99=${P99}s"
        fi
    fi
done

echo ""

# Check for blocking patterns in miner logs
echo -e "${BLUE}Checking miner logs for blocking patterns...${NC}"
echo ""

# Check for "waiting for" patterns
echo -e "${YELLOW}Checking for 'waiting' patterns:${NC}"
kubectl logs -l app=miner --tail=200 2>/dev/null | grep -i "waiting" | tail -10 || echo "  (none found)"
echo ""

# Check for timing issues
echo -e "${YELLOW}Checking for timing/window issues:${NC}"
kubectl logs -l app=miner --tail=200 2>/dev/null | grep -iE "insufficient time|window.*close|timeout" | tail -10 || echo "  (none found)"
echo ""

# Check for claim/proof submissions
echo -e "${YELLOW}Recent claim submissions:${NC}"
kubectl logs -l app=miner --tail=200 2>/dev/null | grep -i "claim.*submit\|session claimed" | tail -5 || echo "  (none found)"
echo ""

# Check for any errors
echo -e "${YELLOW}Any errors:${NC}"
kubectl logs -l app=miner --tail=200 2>/dev/null | grep -iE "error|failed" | tail -10 || echo "  (none found)"
echo ""

# Instructions for monitoring
echo -e "${BLUE}========================================${NC}"
echo -e "${BLUE}Monitoring Commands${NC}"
echo -e "${BLUE}========================================${NC}"
echo ""
echo "Watch miner logs for claim processing:"
echo -e "  ${GREEN}kubectl logs -f -l app=miner 2>/dev/null | grep -E 'claim|proof|waiting|block'${NC}"
echo ""
echo "Check session states:"
echo -e "  ${GREEN}pocket-relay-miner redis sessions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj${NC}"
echo ""
echo "Check Redis streams (WAL):"
echo -e "  ${GREEN}pocket-relay-miner redis streams --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj${NC}"
echo ""
echo "Watch claim window timing:"
echo -e "  ${GREEN}kubectl logs -f -l app=miner 2>/dev/null | grep -E 'claim_window|insufficient|blocks_remaining'${NC}"
echo ""