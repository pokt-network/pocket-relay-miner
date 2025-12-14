#!/usr/bin/env bash
#
# Orchestration script to run all integration tests
# Runs tests in sequence and provides summary report
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"

# Test configuration
START_TILT=false
SKIP_LOAD=false
QUICK=false

# Quick mode settings
QUICK_RELAY_COUNT=50
QUICK_LOAD_COUNT=1000
QUICK_LOAD_CONCURRENCY=50

# Normal mode settings
NORMAL_RELAY_COUNT=100
NORMAL_LOAD_COUNT=10000
NORMAL_LOAD_CONCURRENCY=100

# Parse command-line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
    --start-tilt)
        START_TILT=true
        shift
        ;;
    --skip-load)
        SKIP_LOAD=true
        shift
        ;;
    --quick)
        QUICK=true
        shift
        ;;
    --help)
        echo "Usage: $0 [options]"
        echo
        echo "Options:"
        echo "  --start-tilt    Start Tilt if not running (not implemented)"
        echo "  --skip-load     Skip load test"
        echo "  --quick         Use minimal counts for faster testing"
        echo "  --help          Show this help message"
        echo
        echo "Test sequence:"
        echo "  1. Wait for Tilt deployment to be ready"
        echo "  2. Run simple relay test (all protocols)"
        echo "  3. Run load test (unless --skip-load)"
        echo "  4. Run claim+proof lifecycle test"
        echo
        echo "Examples:"
        echo "  $0                    # Run all tests with normal settings"
        echo "  $0 --quick            # Run all tests with reduced counts"
        echo "  $0 --skip-load        # Skip load test, run others"
        exit 0
        ;;
    *)
        log_error "Unknown option: $1"
        exit 1
        ;;
    esac
done

# Test results tracking
declare -A test_results
test_start_time=$(timestamp_now)

# Run a test and track results
run_test() {
    local test_name=$1
    shift
    local test_script=$1
    shift

    print_header "$test_name"
    echo

    local test_start=$(timestamp_now)

    if "$SCRIPT_DIR/$test_script" "$@"; then
        local test_end=$(timestamp_now)
        local test_duration=$((test_end - test_start))
        test_results["$test_name"]="PASS|$test_duration"
        log_success "$test_name passed ($(format_duration $((test_duration * 1000))))"
        return 0
    else
        local test_end=$(timestamp_now)
        local test_duration=$((test_end - test_start))
        test_results["$test_name"]="FAIL|$test_duration"
        log_error "$test_name failed ($(format_duration $((test_duration * 1000))))"
        return 1
    fi
}

# Generate summary report
generate_summary() {
    local total_duration=$(elapsed_time $test_start_time)

    print_header "Integration Test Summary"
    echo

    local total=0
    local passed=0
    local failed=0

    # Print results table
    printf "%-40s %-10s %-15s\n" "Test" "Status" "Duration"
    print_separator

    for test_name in "${!test_results[@]}"; do
        IFS='|' read -r status duration <<<"${test_results[$test_name]}"

        total=$((total + 1))

        if [ "$status" = "PASS" ]; then
            passed=$((passed + 1))
            printf "%-40s ${COLOR_GREEN}%-10s${COLOR_RESET} %-15s\n" "$test_name" "✅ PASS" "$(format_duration $((duration * 1000)))"
        else
            failed=$((failed + 1))
            printf "%-40s ${COLOR_RED}%-10s${COLOR_RESET} %-15s\n" "$test_name" "❌ FAIL" "$(format_duration $((duration * 1000)))"
        fi
    done

    print_separator
    echo
    echo "Total tests: $total"
    echo -e "${COLOR_GREEN}Passed: $passed${COLOR_RESET}"
    echo -e "${COLOR_RED}Failed: $failed${COLOR_RESET}"
    echo "Total duration: $(format_duration $((total_duration * 1000)))"
    echo

    if [ $failed -eq 0 ]; then
        log_success "All integration tests passed!"
        return 0
    else
        log_error "$failed test(s) failed"
        return 1
    fi
}

# Main script
main() {
    print_header "Pocket RelayMiner Integration Tests"

    if [ "$QUICK" = true ]; then
        log_info "Running in QUICK mode (reduced counts)"
    fi

    if [ "$SKIP_LOAD" = true ]; then
        log_warn "Skipping load test"
    fi

    echo

    # Check if Tilt should be started
    if [ "$START_TILT" = true ]; then
        log_warn "--start-tilt is not implemented yet"
        log_warn "Please start Tilt manually with: tilt up"
        echo
    fi

    # Test 1: Tilt readiness
    run_test "Tilt Deployment Readiness" test-tilt-ready.sh --timeout 300 || {
        log_error "Tilt is not ready, aborting tests"
        generate_summary
        exit 1
    }

    echo

    # Test 2: Simple relay test
    run_test "Simple Relay Test (All Protocols)" test-simple-relay.sh || {
        log_error "Simple relay test failed, continuing with remaining tests..."
    }

    echo

    # Test 3: Load test (optional)
    if [ "$SKIP_LOAD" = false ]; then
        if [ "$QUICK" = true ]; then
            run_test "Load Test (Quick)" test-load-relay.sh \
                --count $QUICK_LOAD_COUNT \
                --concurrency $QUICK_LOAD_CONCURRENCY || {
                log_error "Load test failed, continuing with remaining tests..."
            }
        else
            run_test "Load Test" test-load-relay.sh \
                --count $NORMAL_LOAD_COUNT \
                --concurrency $NORMAL_LOAD_CONCURRENCY || {
                log_error "Load test failed, continuing with remaining tests..."
            }
        fi

        echo
    fi

    # Test 4: Claim+proof lifecycle
    if [ "$QUICK" = true ]; then
        run_test "Claim+Proof Lifecycle Test (Quick)" test-claim-proof.sh \
            --relay-count $QUICK_RELAY_COUNT || {
            log_error "Claim+proof test failed"
        }
    else
        run_test "Claim+Proof Lifecycle Test" test-claim-proof.sh \
            --relay-count $NORMAL_RELAY_COUNT || {
            log_error "Claim+proof test failed"
        }
    fi

    echo

    # Generate and display summary
    if generate_summary; then
        exit 0
    else
        exit 1
    fi
}

main "$@"
