#!/usr/bin/env bash
#
# Test script to run load tests on relay endpoints
# Supports configurable concurrency and request counts
#
# Genesis has 4 services, each with a corresponding app:
#   - develop-http      -> app1 (JSON-RPC/HTTP)
#   - develop-websocket -> app2 (WebSocket)
#   - develop-stream    -> app3 (Streaming/SSE)
#   - develop-grpc      -> app4 (gRPC)
#
# When using --localnet, the correct app key is auto-selected based on service.
# Gateway mode is enabled by default for localnet (matches PATH's signing approach).
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"

# Default parameters
PROTOCOL="jsonrpc"
COUNT=10000
CONCURRENCY=100
# develop-http is the default for JSON-RPC testing (most common)
SERVICE="develop-http"
METRICS_URL="http://localhost:9090/metrics"

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
    --protocol)
        PROTOCOL=$2
        shift 2
        ;;
    --count | -n)
        COUNT=$2
        shift 2
        ;;
    --concurrency | -c)
        CONCURRENCY=$2
        shift 2
        ;;
    --service)
        SERVICE=$2
        shift 2
        ;;
    --help)
        echo "Usage: $0 [options]"
        echo
        echo "Options:"
        echo "  --protocol <name>       Protocol to test (default: jsonrpc)"
        echo "                          Options: jsonrpc, websocket, grpc, stream"
        echo "  --count, -n <num>       Number of requests (default: 10000)"
        echo "  --concurrency, -c <num> Concurrent workers (default: 100)"
        echo "  --service <id>          Service ID (default: develop-http)"
        echo "                          Localnet services: develop-http, develop-websocket, develop-stream, develop-grpc"
        echo "  --help                  Show this help message"
        echo
        echo "Examples:"
        echo "  $0                                         # 10k requests, 100 workers, jsonrpc"
        echo "  $0 --count 50000 --concurrency 200         # 50k requests, 200 workers"
        echo "  $0 --protocol websocket --service develop-websocket  # WebSocket on websocket service"
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

# Get metrics value
# Returns empty string on failure (doesn't exit script)
get_metric_value() {
    local metric_name=$1
    curl -s "$METRICS_URL" 2>/dev/null | grep "^$metric_name " | awk '{print $2}' | head -1 || true
}

# Main script
main() {
    print_header "Load Test"

    # Check required commands
    require_commands "$MINER_BIN" curl || exit 1

    # Display configuration
    log_info "Configuration:"
    echo "  Protocol:    $PROTOCOL"
    echo "  Requests:    $COUNT"
    echo "  Concurrency: $CONCURRENCY"
    echo "  Service:     $SERVICE"
    echo

    # Capture metrics before test
    log_info "Capturing baseline metrics..."
    local metrics_before
    metrics_before=$(get_metric_value "ha_relayer_requests_total")
    if [ -z "$metrics_before" ]; then
        log_warn "Could not fetch baseline metrics (relayer may not have metrics yet)"
        metrics_before="0"
    fi

    # Run load test
    print_header "Running Load Test"
    local start_time=$(timestamp_now)

    log_info "Starting load test..."
    echo

    # Run the relay command with load test flag
    local output
    local exit_code=0
    output=$("$MINER_BIN" relay "$PROTOCOL" \
        --localnet \
        --service "$SERVICE" \
        --load-test \
        -n "$COUNT" \
        --concurrency "$CONCURRENCY" \
        --timeout 300 2>&1) || exit_code=$?

    local end_time=$(timestamp_now)
    local duration=$((end_time - start_time))

    echo
    if [ $exit_code -ne 0 ]; then
        log_error "Load test failed with exit code: $exit_code"
        echo
        echo "Output:"
        echo "$output"
        exit 1
    fi

    log_success "Load test completed in $(format_duration $((duration * 1000)))"
    echo

    # Parse output for metrics
    # The relay command outputs a summary with metrics
    print_header "Load Test Results"
    echo

    # Show the output which contains the metrics summary
    echo "$output" | grep -A 50 "Load Test Summary\|Summary:" || echo "$output"

    echo
    print_separator

    # Query Prometheus metrics after test
    log_info "Querying Prometheus metrics..."
    local metrics_after
    metrics_after=$(get_metric_value "ha_relayer_requests_total")

    if [ -n "$metrics_after" ] && [ "$metrics_after" != "0" ]; then
        local requests_processed=$((metrics_after - metrics_before))
        log_info "Relayer processed: $requests_processed requests (delta)"
    else
        log_warn "Could not fetch post-test metrics"
    fi

    echo
    log_success "Load test completed successfully"
    exit 0
}

main "$@"
