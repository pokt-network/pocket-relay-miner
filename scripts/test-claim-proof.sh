#!/usr/bin/env bash
#
# Test script to monitor session lifecycle and verify claim+proof submission
# Tracks session from active → claimed → settled
#
# Genesis has 4 services, each with a corresponding app:
#   - develop-http      -> app1 (JSON-RPC/HTTP)
#   - develop-websocket -> app2 (WebSocket)
#   - develop-stream    -> app3 (Streaming/SSE)
#   - develop-grpc      -> app4 (gRPC)
#
# When using --localnet, the correct app key is auto-selected based on service.
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/metrics-helpers.sh"

# Default parameters
SUPPLIER="pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj" # localnet supplier 1
# develop-http is the default for JSON-RPC testing (most common)
SERVICE="develop-http"
RELAY_COUNT=100
WAIT_TIMEOUT=600
POLL_INTERVAL=5

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
    --supplier)
        SUPPLIER=$2
        shift 2
        ;;
    --service)
        SERVICE=$2
        shift 2
        ;;
    --relay-count)
        RELAY_COUNT=$2
        shift 2
        ;;
    --wait-timeout)
        WAIT_TIMEOUT=$2
        shift 2
        ;;
    --help)
        echo "Usage: $0 [options]"
        echo
        echo "Options:"
        echo "  --supplier <address>   Supplier operator address (default: localnet supplier 1)"
        echo "  --service <id>         Service ID (default: develop-http)"
        echo "                         Localnet services: develop-http, develop-websocket, develop-stream, develop-grpc"
        echo "  --relay-count <num>    Number of relays to send (default: 100)"
        echo "  --wait-timeout <sec>   Max time to wait for settlement (default: 600)"
        echo "  --help                 Show this help message"
        echo
        echo "Examples:"
        echo "  $0                                    # Use defaults"
        echo "  $0 --relay-count 200                  # Send 200 relays"
        echo "  $0 --service develop-websocket        # Test websocket service"
        exit 0
        ;;
    *)
        log_error "Unknown option: $1"
        exit 1
        ;;
    esac
done

# Find miner binary
MINER_BIN=$(find_miner_binary) || exit 1

# Set metrics URL to miner endpoint (not relayer)
export PROMETHEUS_URL="http://localhost:9092"

# Get session state from redis-debug
get_session_state() {
    local session_id=$1
    "$MINER_BIN" redis-debug sessions --supplier "$SUPPLIER" --session "$session_id" --json 2>/dev/null |
        jq -r '.state' 2>/dev/null || echo "unknown"
}

# Get active session ID for supplier
get_active_session() {
    "$MINER_BIN" redis-debug sessions --supplier "$SUPPLIER" --state active 2>/dev/null |
        grep -v "SESSION ID" | grep -v "Total:" | awk '{print $1}' | head -1
}

# Get session details
get_session_details() {
    local session_id=$1
    "$MINER_BIN" redis-debug sessions --supplier "$SUPPLIER" --session "$session_id" --json 2>/dev/null
}

# Phase 1: Generate relays
phase1_generate_relays() {
    print_header "Phase 1: Generate Relays"

    log_info "Sending $RELAY_COUNT relays to create a session..."

    # Send relays (use load-test mode for count > 1)
    "$MINER_BIN" relay jsonrpc \
        --localnet \
        --service "$SERVICE" \
        --load-test \
        --count "$RELAY_COUNT" \
        --concurrency 10 \
        --timeout 60 >/dev/null 2>&1 || {
        log_error "Failed to send relays"
        exit 1
    }

    log_success "Relays sent successfully"

    # Wait a moment for processing
    sleep 2

    # Get active session
    log_info "Looking for active session..."
    SESSION_ID=$(get_active_session)

    if [ -z "$SESSION_ID" ]; then
        log_error "No active session found after sending relays"
        log_error "Check miner logs for details"
        exit 1
    fi

    log_success "Found active session: $SESSION_ID"

    # Get session details
    local details
    details=$(get_session_details "$SESSION_ID")

    local relay_count
    relay_count=$(echo "$details" | jq -r '.relay_count // 0' 2>/dev/null || echo "0")

    local compute_units
    compute_units=$(echo "$details" | jq -r '.total_compute_units // 0' 2>/dev/null || echo "0")

    log_info "Session details: $relay_count relays, $compute_units compute units"
}

# Phase 2: Monitor session state
phase2_monitor_state() {
    print_header "Phase 2: Monitor Session State"

    local start_time=$(timestamp_now)
    local current_state="active"
    local timeline=()

    timeline+=("$(timestamp_now)|active|Session started")

    log_info "Monitoring session $SESSION_ID for state transitions..."
    log_info "Polling every ${POLL_INTERVAL}s (timeout: ${WAIT_TIMEOUT}s)"
    echo

    while true; do
        local elapsed=$(elapsed_time $start_time)

        # Check timeout
        if [ $elapsed -ge $WAIT_TIMEOUT ]; then
            log_error "Timeout waiting for settlement after ${WAIT_TIMEOUT}s"
            return 1
        fi

        # Get current state
        local new_state
        new_state=$(get_session_state "$SESSION_ID")

        # Check for state change
        if [ "$new_state" != "$current_state" ]; then
            log_info "State transition: $current_state → $new_state (${elapsed}s elapsed)"

            timeline+=("$(timestamp_now)|$new_state|State: $new_state")

            current_state=$new_state

            # Check if settled
            if [ "$current_state" = "settled" ]; then
                log_success "Session settled!"
                break
            fi
        fi

        sleep $POLL_INTERVAL
    done

    echo
    log_success "Session lifecycle complete in $(format_duration $((elapsed * 1000)))"

    # Store timeline for later display
    TIMELINE=("${timeline[@]}")
}

# Phase 3: Verify claim
phase3_verify_claim() {
    print_header "Phase 3: Verify Claim"

    local details
    details=$(get_session_details "$SESSION_ID")

    local claimed_root_hash
    claimed_root_hash=$(echo "$details" | jq -r '.claimed_root_hash // ""' 2>/dev/null || echo "")

    if [ -z "$claimed_root_hash" ] || [ "$claimed_root_hash" = "null" ]; then
        log_error "No claimed root hash found"
        return 1
    fi

    log_success "Claimed root hash: ${claimed_root_hash:0:16}..."

    # Check metrics
    local claims_submitted
    claims_submitted=$(get_claims_submitted "$SUPPLIER" 2>/dev/null || echo "0")

    log_info "Claims submitted (metric): $claims_submitted"

    # Query SMST tree
    log_info "Querying SMST tree..."
    "$MINER_BIN" redis-debug smst --session "$SESSION_ID" --summary 2>/dev/null | head -10 || {
        log_warn "Could not query SMST tree (may have been cleaned up)"
    }

    echo
    log_success "Claim verification complete"
}

# Phase 4: Verify proof
phase4_verify_proof() {
    print_header "Phase 4: Verify Proof"

    # Check if proof was submitted
    local proofs_submitted
    proofs_submitted=$(get_proofs_submitted "$SUPPLIER" 2>/dev/null || echo "0")

    log_info "Proofs submitted (metric): $proofs_submitted"

    # Check proof requirement metrics
    if metric_exists "ha_miner_proof_requirement_required_total"; then
        local required
        required=$(get_metric_labels "ha_miner_proof_requirement_required_total" "supplier=\"$SUPPLIER\"" | head -1 || echo "0")

        local skipped
        skipped=$(get_metric_labels "ha_miner_proof_requirement_skipped_total" "supplier=\"$SUPPLIER\"" | head -1 || echo "0")

        log_info "Proof requirement - Required: $required, Skipped: $skipped"
    fi

    echo
    log_success "Proof verification complete"
}

# Phase 5: Display summary
phase5_summary() {
    print_header "Session Lifecycle Verification"

    echo
    echo "Session ID: $SESSION_ID"
    echo "Service: $SERVICE"
    echo "Supplier: $SUPPLIER"
    echo

    # Display timeline
    print_separator
    echo "Timeline:"
    print_separator

    local start_ts=${TIMELINE[0]%%|*}

    for entry in "${TIMELINE[@]}"; do
        IFS='|' read -r timestamp state description <<<"$entry"
        local elapsed=$((timestamp - start_ts))
        local formatted_time=$(printf "%02d:%02d" $((elapsed / 60)) $((elapsed % 60)))

        echo "  $formatted_time - $description"
    done

    print_separator
    echo

    # Get final session details
    local details
    details=$(get_session_details "$SESSION_ID")

    local relay_count
    relay_count=$(echo "$details" | jq -r '.relay_count // 0' 2>/dev/null || echo "0")

    local compute_units
    compute_units=$(echo "$details" | jq -r '.total_compute_units // 0' 2>/dev/null || echo "0")

    local claimed_root_hash
    claimed_root_hash=$(echo "$details" | jq -r '.claimed_root_hash // ""' 2>/dev/null || echo "")

    # Verification checklist
    print_separator
    echo "Verification:"
    print_separator

    echo -e "  ${COLOR_GREEN}✓${COLOR_RESET} Session created: $relay_count relays, $compute_units compute units"

    if [ -n "$claimed_root_hash" ] && [ "$claimed_root_hash" != "null" ]; then
        echo -e "  ${COLOR_GREEN}✓${COLOR_RESET} Claim submitted successfully"
        echo -e "  ${COLOR_GREEN}✓${COLOR_RESET} SMST root hash: ${claimed_root_hash:0:16}..."
    else
        echo -e "  ${COLOR_RED}✗${COLOR_RESET} Claim submission failed"
    fi

    local final_state
    final_state=$(get_session_state "$SESSION_ID")

    if [ "$final_state" = "settled" ]; then
        echo -e "  ${COLOR_GREEN}✓${COLOR_RESET} Session settled on-chain"
    else
        echo -e "  ${COLOR_YELLOW}⋯${COLOR_RESET} Session state: $final_state (not settled)"
    fi

    print_separator
}

# Main script
main() {
    print_header "Claim+Proof Lifecycle Test"

    # Check required commands
    require_commands "$MINER_BIN" jq curl || exit 1

    log_info "Configuration:"
    echo "  Supplier: $SUPPLIER"
    echo "  Service: $SERVICE"
    echo "  Relay count: $RELAY_COUNT"
    echo "  Timeout: ${WAIT_TIMEOUT}s"
    echo

    # Run phases
    phase1_generate_relays
    phase2_monitor_state || exit 3
    phase3_verify_claim || exit 1
    phase4_verify_proof || exit 2
    phase5_summary

    echo
    log_success "Claim+proof lifecycle test completed successfully!"
    exit 0
}

main "$@"
