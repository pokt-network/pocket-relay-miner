#!/usr/bin/env bash
#
# Test script to send simple relay requests on each protocol
# Verifies basic relay functionality across all transports
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

# Default parameters
# develop-http is the default for JSON-RPC testing (most common)
SERVICE="develop-http"
PROTOCOLS="jsonrpc,websocket,grpc,stream"

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
    --service)
        SERVICE=$2
        shift 2
        ;;
    --protocols)
        PROTOCOLS=$2
        shift 2
        ;;
    --help)
        echo "Usage: $0 [options]"
        echo
        echo "Options:"
        echo "  --service <id>        Service ID (default: develop-http)"
        echo "                        Localnet services: develop-http, develop-websocket, develop-stream, develop-grpc"
        echo "  --protocols <list>    Comma-separated protocol list (default: jsonrpc,websocket,grpc,stream)"
        echo "  --help                Show this help message"
        echo
        echo "Examples:"
        echo "  $0                                    # Test all protocols on develop-http"
        echo "  $0 --service develop-websocket        # Test all protocols on websocket service"
        echo "  $0 --protocols jsonrpc,websocket      # Test only HTTP and WebSocket"
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

# Test a single protocol
# Returns 0 on success, 1 on failure
# Outputs: protocol status response_time_ms
test_protocol() {
    local protocol=$1
    local start_ms=$(date +%s%3N)

    log_info "Testing $protocol..."

    # Run relay command
    local output
    local exit_code=0
    output=$("$MINER_BIN" relay "$protocol" \
        --localnet \
        --service "$SERVICE" \
        --count 1 \
        --timeout 30 2>&1) || exit_code=$?

    local end_ms=$(date +%s%3N)
    local duration_ms=$((end_ms - start_ms))

    if [ $exit_code -eq 0 ]; then
        log_success "$protocol succeeded (${duration_ms}ms)"
        echo "$protocol|success|$duration_ms"
        return 0
    else
        log_error "$protocol failed (exit code: $exit_code)"
        log_error "Output: $output"
        echo "$protocol|failed|$duration_ms"
        return 1
    fi
}

# Main script
main() {
    print_header "Simple Relay Test"

    # Check required commands
    require_commands "$MINER_BIN" || exit 1

    log_info "Service: $SERVICE"
    log_info "Protocols: $PROTOCOLS"
    echo

    # Convert comma-separated protocols to array
    IFS=',' read -ra PROTOCOL_ARRAY <<<"$PROTOCOLS"

    # Test each protocol
    local results=()
    local total=0
    local success=0
    local failed=0

    for protocol in "${PROTOCOL_ARRAY[@]}"; do
        total=$((total + 1))
        local result
        if result=$(test_protocol "$protocol"); then
            results+=("$result")
            success=$((success + 1))
        else
            results+=("$result")
            failed=$((failed + 1))
        fi
        echo
    done

    # Print results table
    print_header "Results Summary"
    echo
    printf "%-15s %-10s %-15s\n" "Protocol" "Status" "Response Time"
    print_separator

    for result in "${results[@]}"; do
        IFS='|' read -r protocol status duration <<<"$result"

        if [ "$status" = "success" ]; then
            printf "%-15s ${COLOR_GREEN}%-10s${COLOR_RESET} %-15s\n" "$protocol" "✅ OK" "$(format_duration "$duration")"
        else
            printf "%-15s ${COLOR_RED}%-10s${COLOR_RESET} %-15s\n" "$protocol" "❌ FAIL" "$(format_duration "$duration")"
        fi
    done

    echo
    print_separator
    echo -e "Total: $total  |  ${COLOR_GREEN}Success: $success${COLOR_RESET}  |  ${COLOR_RED}Failed: $failed${COLOR_RESET}"
    print_separator
    echo

    if [ $failed -eq 0 ]; then
        log_success "All protocols passed!"
        exit 0
    else
        log_error "$failed protocol(s) failed"
        exit 1
    fi
}

main "$@"
