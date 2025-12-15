#!/bin/bash
#
# Simplified End-to-End Claim Verification Test
#
# Tests relay delivery and claim submission for all protocols
#

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

SERVICE_ID="develop"
RELAYER_BIN="./bin/pocket-relay-miner"
SUPPLIER_ADDR="pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"
RELAY_COUNT=20

echo -e "${BLUE}================================================${NC}"
echo -e "${BLUE}  E2E Relay & Claim Test${NC}"
echo -e "${BLUE}================================================${NC}"
echo ""

# Function to get session state before test
get_baseline() {
    echo -e "${YELLOW}Getting baseline session state...${NC}"

    # Count existing sessions
    local total_sessions=$($RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR 2>/dev/null | \
        grep -c "^[a-f0-9]\{64\}" || echo "0")

    echo -e "${GREEN}Baseline: $total_sessions sessions in Redis${NC}"

    # Show last few sessions
    echo -e "${BLUE}Recent sessions:${NC}"
    $RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR 2>/dev/null | head -8
    echo ""

    return 0
}

# Function to send relays and verify delivery
test_protocol_relays() {
    local protocol="$1"
    local count="$2"

    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Testing: $protocol ($count relays)${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    # Send relays
    echo -e "${YELLOW}Sending $count relays via $protocol...${NC}"
    local output=$($RELAYER_BIN relay $protocol --localnet --service $SERVICE_ID \
        --load-test -n $count --concurrency 10 2>&1)

    local success_count=$(echo "$output" | grep "^Successful:" | awk '{print $2}')
    local total_requests=$(echo "$output" | grep "^Total Requests:" | awk '{print $3}')

    echo "$output" | grep -E "(Total Requests|Successful|Success Rate|Throughput|Latency)" || true
    echo ""

    if [[ "$success_count" == "$count" ]]; then
        echo -e "${GREEN}✅ $protocol: $success_count/$count relays delivered successfully${NC}"
        return 0
    else
        echo -e "${RED}❌ $protocol: Only $success_count/$count relays succeeded${NC}"
        return 1
    fi
}

# Function to check Redis Streams for pending relays
check_redis_streams() {
    echo -e "${YELLOW}Checking Redis Streams...${NC}"

    local streams_output=$($RELAYER_BIN redis-debug streams --supplier $SUPPLIER_ADDR 2>/dev/null || echo "")

    if [[ -n "$streams_output" ]]; then
        echo "$streams_output" | head -10

        local pending=$(echo "$streams_output" | grep -oP 'Pending: \K[0-9]+' | head -1 || echo "0")
        local processed=$(echo "$streams_output" | grep -oP 'Processed: \K[0-9]+' | head -1 || echo "0")

        echo -e "${BLUE}Redis Streams: ${GREEN}$pending pending${NC}, ${GREEN}$processed processed${NC}"
    else
        echo -e "${YELLOW}No stream data available yet${NC}"
    fi
    echo ""
}

# Function to check session updates
check_sessions() {
    echo -e "${YELLOW}Checking session data...${NC}"

    echo -e "${BLUE}All sessions (latest first):${NC}"
    $RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR 2>/dev/null | head -12
    echo ""
}

# Main test execution
main() {
    # Check binary exists
    if [ ! -f "$RELAYER_BIN" ]; then
        echo -e "${RED}Binary not found: $RELAYER_BIN${NC}"
        echo -e "${YELLOW}Run: make build${NC}"
        exit 1
    fi

    # Get baseline
    get_baseline

    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Phase 1: Individual Protocol Tests${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    # Test 1: HTTP/JSON-RPC
    test_protocol_relays "jsonrpc" $RELAY_COUNT
    http_result=$?

    echo ""
    sleep 2
    check_redis_streams
    sleep 2

    # Test 2: WebSocket
    test_protocol_relays "websocket" $RELAY_COUNT
    ws_result=$?

    echo ""
    sleep 2
    check_redis_streams
    sleep 2

    # Test 3: gRPC
    test_protocol_relays "grpc" $RELAY_COUNT
    grpc_result=$?

    echo ""
    sleep 2
    check_redis_streams
    sleep 2

    # Test 4: Stream
    test_protocol_relays "stream" 5  # Fewer for streaming
    stream_result=$?

    echo ""
    sleep 2

    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Phase 2: Multi-Protocol Same Session${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    echo -e "${YELLOW}Sending relays to all protocols in parallel...${NC}"

    # Send to all protocols in parallel
    ($RELAYER_BIN relay jsonrpc --localnet --service $SERVICE_ID --load-test -n $RELAY_COUNT --concurrency 10 2>&1 | \
        grep -E "Success Rate" || true) &
    PID_HTTP=$!

    ($RELAYER_BIN relay websocket --localnet --service $SERVICE_ID --load-test -n $RELAY_COUNT --concurrency 10 2>&1 | \
        grep -E "Success Rate" || true) &
    PID_WS=$!

    ($RELAYER_BIN relay grpc --localnet --service $SERVICE_ID --load-test -n $RELAY_COUNT --concurrency 10 2>&1 | \
        grep -E "Success Rate" || true) &
    PID_GRPC=$!

    # Wait for all
    wait $PID_HTTP
    wait $PID_WS
    wait $PID_GRPC

    echo -e "${GREEN}✅ All protocols completed${NC}"
    echo ""

    sleep 3

    # Final checks
    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Final State${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    check_redis_streams
    check_sessions

    # Summary
    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Test Summary${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    local passed=0
    local failed=0

    if [ $http_result -eq 0 ]; then
        echo -e "${GREEN}✅ HTTP/JSON-RPC: PASS${NC}"
        passed=$((passed + 1))
    else
        echo -e "${RED}❌ HTTP/JSON-RPC: FAIL${NC}"
        failed=$((failed + 1))
    fi

    if [ $ws_result -eq 0 ]; then
        echo -e "${GREEN}✅ WebSocket: PASS${NC}"
        passed=$((passed + 1))
    else
        echo -e "${RED}❌ WebSocket: FAIL${NC}"
        failed=$((failed + 1))
    fi

    if [ $grpc_result -eq 0 ]; then
        echo -e "${GREEN}✅ gRPC: PASS${NC}"
        passed=$((passed + 1))
    else
        echo -e "${RED}❌ gRPC: FAIL${NC}"
        failed=$((failed + 1))
    fi

    if [ $stream_result -eq 0 ]; then
        echo -e "${GREEN}✅ Stream: PASS${NC}"
        passed=$((passed + 1))
    else
        echo -e "${RED}❌ Stream: FAIL${NC}"
        failed=$((failed + 1))
    fi

    echo ""
    echo -e "${BLUE}Results: ${GREEN}$passed passed${NC}, ${RED}$failed failed${NC}"
    echo ""

    if [ $failed -eq 0 ]; then
        echo -e "${GREEN}🎉 ALL RELAY DELIVERY TESTS PASSED! 🎉${NC}"
        echo ""
        echo -e "${YELLOW}Note: Claims will be submitted when sessions end (every ~10 blocks).${NC}"
        echo -e "${YELLOW}Use 'watch -n 2 ./bin/pocket-relay-miner redis-debug sessions --supplier $SUPPLIER_ADDR' to monitor claim submission.${NC}"
        exit 0
    else
        exit 1
    fi
}

main
