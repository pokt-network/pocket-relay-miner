#!/bin/bash
#
# End-to-End Claim Verification Test
#
# Tests the full relay mining pipeline:
# 1. HTTP: 20 relays → verify claim
# 2. gRPC: 20 relays (next session) → verify claim
# 3. WebSocket: 20 relays (next session) → verify claim
# 4. Stream: 20 relays (next session) → verify claim
# 5. All protocols: 20 relays each (same session) → verify single claim with 80 relays
#

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
SERVICE_ID="develop"
RELAYER_BIN="./bin/pocket-relay-miner"
SUPPLIER_ADDR="pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj"
NODE_GRPC="localhost:9090"
CHAIN_ID="poktroll"

# Test state
CURRENT_SESSION=""
CURRENT_SESSION_START=""
CURRENT_SESSION_END=""
TEST_RESULTS=()

echo -e "${BLUE}================================================${NC}"
echo -e "${BLUE}  End-to-End Relay Mining Pipeline Test${NC}"
echo -e "${BLUE}================================================${NC}"
echo ""

# Function to get current block height
get_current_height() {
    $RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --list 2>/dev/null | \
        grep -oP 'current_height=\K[0-9]+' | head -1 || echo "0"
}

# Function to get current session info
get_session_info() {
    local output
    output=$($RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --list 2>/dev/null || echo "")

    if [[ -z "$output" ]]; then
        echo -e "${RED}Failed to get session info${NC}"
        return 1
    fi

    # Parse session info (format: session_id, start_height, end_height, state)
    CURRENT_SESSION=$(echo "$output" | grep -oP 'session_id=[a-f0-9]+' | head -1 | cut -d= -f2)
    CURRENT_SESSION_START=$(echo "$output" | grep -oP 'start_height=\K[0-9]+' | head -1)
    CURRENT_SESSION_END=$(echo "$output" | grep -oP 'end_height=\K[0-9]+' | head -1)

    echo -e "${GREEN}Current Session:${NC} $CURRENT_SESSION"
    echo -e "${GREEN}Session Range:${NC} $CURRENT_SESSION_START → $CURRENT_SESSION_END"
}

# Function to wait for next session
wait_for_next_session() {
    local old_session="$1"
    echo -e "${YELLOW}Waiting for next session...${NC}"

    local max_wait=300  # 5 minutes max
    local elapsed=0

    while [ $elapsed -lt $max_wait ]; do
        sleep 2
        elapsed=$((elapsed + 2))

        get_session_info > /dev/null 2>&1

        if [[ "$CURRENT_SESSION" != "$old_session" && -n "$CURRENT_SESSION" ]]; then
            echo -e "${GREEN}New session started: $CURRENT_SESSION${NC}"
            return 0
        fi

        # Show progress every 10 seconds
        if [ $((elapsed % 10)) -eq 0 ]; then
            local current_height=$(get_current_height)
            echo -e "${BLUE}  Still in session $old_session... (height: $current_height, target: $CURRENT_SESSION_END)${NC}"
        fi
    done

    echo -e "${RED}Timeout waiting for next session${NC}"
    return 1
}

# Function to wait for session to end and claim to be submitted
wait_for_claim() {
    local session_id="$1"
    local expected_relays="$2"
    local protocol_name="$3"

    echo -e "${YELLOW}Waiting for session $session_id to end and claim to be submitted...${NC}"

    local max_wait=300  # 5 minutes max
    local elapsed=0

    while [ $elapsed -lt $max_wait ]; do
        sleep 3
        elapsed=$((elapsed + 3))

        # Check session state
        local session_state=$($RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --session $session_id 2>/dev/null | \
            grep -oP 'state=\K\w+' | head -1 || echo "unknown")

        # Check if claim was submitted
        if [[ "$session_state" == "claimed" ]]; then
            echo -e "${GREEN}✅ Claim submitted for session $session_id!${NC}"

            # Get relay count from Redis
            local relay_count=$($RELAYER_BIN redis-debug sessions --supplier $SUPPLIER_ADDR --session $session_id 2>/dev/null | \
                grep -oP 'relay_count=\K[0-9]+' | head -1 || echo "0")

            echo -e "${GREEN}Relay count in session: $relay_count${NC}"

            if [ "$relay_count" -ge "$expected_relays" ]; then
                echo -e "${GREEN}✅ PASS: $protocol_name - Found $relay_count relays (expected >= $expected_relays)${NC}"
                TEST_RESULTS+=("PASS: $protocol_name ($relay_count relays)")
                return 0
            else
                echo -e "${RED}❌ FAIL: $protocol_name - Found $relay_count relays (expected >= $expected_relays)${NC}"
                TEST_RESULTS+=("FAIL: $protocol_name ($relay_count relays, expected >= $expected_relays)")
                return 1
            fi
        fi

        # Show progress every 15 seconds
        if [ $((elapsed % 15)) -eq 0 ]; then
            local current_height=$(get_current_height)
            echo -e "${BLUE}  Session state: $session_state (height: $current_height, session end: $CURRENT_SESSION_END)${NC}"
        fi
    done

    echo -e "${RED}❌ FAIL: $protocol_name - Timeout waiting for claim${NC}"
    TEST_RESULTS+=("FAIL: $protocol_name (timeout)")
    return 1
}

# Function to send relays via a protocol
send_relays() {
    local protocol="$1"
    local count="$2"
    local session_id="$3"

    echo -e "${BLUE}Sending $count relays via $protocol (session: $session_id)...${NC}"

    $RELAYER_BIN relay $protocol --localnet --service $SERVICE_ID --load-test -n $count --concurrency 10 2>&1 | \
        grep -E "(Total Requests|Successful|Success Rate|Throughput)" || true

    local exit_code=${PIPESTATUS[0]}
    if [ $exit_code -ne 0 ]; then
        echo -e "${RED}Failed to send relays via $protocol${NC}"
        return 1
    fi

    echo -e "${GREEN}✅ Sent $count relays via $protocol${NC}"

    # Wait a moment for relays to be published to Redis
    sleep 2

    # Verify relays in Redis Streams
    local stream_count=$($RELAYER_BIN redis-debug streams --supplier $SUPPLIER_ADDR 2>/dev/null | \
        grep -oP 'pending=\K[0-9]+' | head -1 || echo "0")
    echo -e "${BLUE}  Redis Streams pending: $stream_count${NC}"

    return 0
}

# Function to run a single protocol test
test_protocol() {
    local protocol="$1"
    local relay_count=20

    echo ""
    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Test: $protocol (20 relays)${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    # Get current session
    get_session_info
    local test_session="$CURRENT_SESSION"

    # Send relays
    if ! send_relays "$protocol" "$relay_count" "$test_session"; then
        TEST_RESULTS+=("FAIL: $protocol (relay send failed)")
        return 1
    fi

    # Wait for claim
    wait_for_claim "$test_session" "$relay_count" "$protocol"

    # Wait for next session before next test
    if ! wait_for_next_session "$test_session"; then
        echo -e "${RED}Failed to proceed to next session${NC}"
        return 1
    fi

    # Cooldown between tests
    sleep 5
}

# Function to run multi-protocol test (all protocols in same session)
test_all_protocols_same_session() {
    local relay_count=20
    local expected_total=80

    echo ""
    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Test: All Protocols (20 relays each)${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    # Get current session
    get_session_info
    local test_session="$CURRENT_SESSION"

    # Send relays to all protocols in parallel
    echo -e "${BLUE}Sending relays to all protocols in parallel...${NC}"

    (send_relays "jsonrpc" "$relay_count" "$test_session") &
    PID_HTTP=$!

    (send_relays "grpc" "$relay_count" "$test_session") &
    PID_GRPC=$!

    (send_relays "websocket" "$relay_count" "$test_session") &
    PID_WS=$!

    (send_relays "stream" "$relay_count" "$test_session") &
    PID_STREAM=$!

    # Wait for all to complete
    wait $PID_HTTP
    local http_result=$?

    wait $PID_GRPC
    local grpc_result=$?

    wait $PID_WS
    local ws_result=$?

    wait $PID_STREAM
    local stream_result=$?

    if [ $http_result -ne 0 ] || [ $grpc_result -ne 0 ] || [ $ws_result -ne 0 ] || [ $stream_result -ne 0 ]; then
        echo -e "${RED}One or more protocols failed to send relays${NC}"
        TEST_RESULTS+=("FAIL: All protocols (relay send failed)")
        return 1
    fi

    echo -e "${GREEN}✅ All protocols sent relays successfully${NC}"

    # Wait for claim
    wait_for_claim "$test_session" "$expected_total" "All Protocols"
}

# Main test execution
main() {
    echo -e "${GREEN}Starting end-to-end claim verification tests...${NC}"
    echo ""

    # Check if binary exists
    if [ ! -f "$RELAYER_BIN" ]; then
        echo -e "${RED}Binary not found: $RELAYER_BIN${NC}"
        echo -e "${YELLOW}Run: make build${NC}"
        exit 1
    fi

    # Test 1: HTTP
    test_protocol "jsonrpc"

    # Test 2: gRPC
    test_protocol "grpc"

    # Test 3: WebSocket
    test_protocol "websocket"

    # Test 4: Stream
    test_protocol "stream"

    # Test 5: All protocols in same session
    test_all_protocols_same_session

    # Print summary
    echo ""
    echo -e "${BLUE}================================================${NC}"
    echo -e "${BLUE}  Test Results Summary${NC}"
    echo -e "${BLUE}================================================${NC}"
    echo ""

    local pass_count=0
    local fail_count=0

    for result in "${TEST_RESULTS[@]}"; do
        if [[ "$result" == PASS* ]]; then
            echo -e "${GREEN}✅ $result${NC}"
            pass_count=$((pass_count + 1))
        else
            echo -e "${RED}❌ $result${NC}"
            fail_count=$((fail_count + 1))
        fi
    done

    echo ""
    echo -e "${BLUE}Total: ${GREEN}$pass_count passed${NC}, ${RED}$fail_count failed${NC}"
    echo ""

    if [ $fail_count -eq 0 ]; then
        echo -e "${GREEN}🎉 ALL TESTS PASSED! 🎉${NC}"
        exit 0
    else
        echo -e "${RED}Some tests failed.${NC}"
        exit 1
    fi
}

# Run main
main
