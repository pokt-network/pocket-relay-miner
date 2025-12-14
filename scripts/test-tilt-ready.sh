#!/usr/bin/env bash
#
# Test script to wait for Tilt deployment to be ready
# Verifies all critical services are up and healthy
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/tilt-helpers.sh"

# Default timeout (5 minutes)
TIMEOUT=300

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
    --timeout)
        TIMEOUT=$2
        shift 2
        ;;
    --help)
        echo "Usage: $0 [options]"
        echo
        echo "Options:"
        echo "  --timeout <seconds>    Timeout for waiting (default: 300)"
        echo "  --help                 Show this help message"
        exit 0
        ;;
    *)
        log_error "Unknown option: $1"
        exit 1
        ;;
    esac
done

# Main script
main() {
    print_header "Tilt Deployment Readiness Check"

    # Check required commands
    require_commands curl jq nc || exit 1

    # Step 1: Wait for Tilt UI
    log_info "Step 1: Checking Tilt UI..."
    if ! wait_for_tilt_ui 60; then
        log_error "Tilt UI is not running"
        log_error "Please start Tilt with: tilt up"
        exit 1
    fi

    # Step 2: Wait for Tilt resources
    log_info "Step 2: Waiting for Tilt resources..."
    if ! wait_for_tilt_ready $TIMEOUT; then
        log_error "Tilt resources failed to become ready"
        echo
        print_header "Resource Status"
        list_tilt_resources
        exit 1
    fi

    # Step 3: Verify critical services
    print_header "Verifying Critical Services"

    local all_ok=true

    # Redis
    if wait_for_port localhost 6379 10; then
        log_success "Redis is ready (port 6379)"
    else
        log_error "Redis is not ready (port 6379)"
        all_ok=false
    fi

    # Validator RPC
    if wait_for_port localhost 26657 10; then
        log_success "Validator RPC is ready (port 26657)"
    else
        log_error "Validator RPC is not ready (port 26657)"
        all_ok=false
    fi

    # Validator gRPC
    if wait_for_port localhost 9090 10; then
        log_success "Validator gRPC is ready (port 9090)"
    else
        log_error "Validator gRPC is not ready (port 9090)"
        all_ok=false
    fi

    # Relayer HTTP
    if wait_for_port localhost 8180 10; then
        log_success "Relayer HTTP is ready (port 8180)"
    else
        log_error "Relayer HTTP is not ready (port 8180)"
        all_ok=false
    fi

    echo
    print_header "Tilt Resources Status"
    list_tilt_resources

    echo
    if [ "$all_ok" = true ]; then
        print_separator
        log_success "All critical services are ready!"
        log_info "You can now run integration tests"
        print_separator
        exit 0
    else
        print_separator
        log_error "Some services are not ready"
        log_error "Check Tilt logs for details: tilt logs"
        print_separator
        exit 1
    fi
}

main "$@"
