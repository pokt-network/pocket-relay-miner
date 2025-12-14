#!/usr/bin/env bash
#
# Tilt helper functions for monitoring deployment status
#

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Tilt API endpoint
TILT_API="http://localhost:10350/api"

# Check if Tilt is running
# Returns 0 if running, 1 if not
check_tilt_running() {
    if curl -s --max-time 2 "$TILT_API/view" >/dev/null 2>&1; then
        return 0
    fi
    return 1
}

# Get Tilt resources status
# Usage: get_tilt_resources
get_tilt_resources() {
    curl -s "$TILT_API/view" 2>/dev/null
}

# Wait for Tilt UI to be responsive
# Usage: wait_for_tilt_ui <timeout_seconds>
wait_for_tilt_ui() {
    local timeout=${1:-60}
    local elapsed=0

    log_info "Waiting for Tilt UI at http://localhost:10350..."

    while ! check_tilt_running; do
        if [ $elapsed -ge $timeout ]; then
            log_error "Timeout waiting for Tilt UI after ${timeout}s"
            return 1
        fi
        sleep 2
        ((elapsed += 2))
    done

    log_success "Tilt UI is responsive"
    return 0
}

# Wait for all Tilt resources to be ready
# Usage: wait_for_tilt_ready <timeout_seconds>
wait_for_tilt_ready() {
    local timeout=${1:-300}
    local start_time=$(timestamp_now)

    log_info "Waiting for Tilt resources to be ready (timeout: ${timeout}s)..."

    while true; do
        local elapsed=$(elapsed_time $start_time)

        if [ $elapsed -ge $timeout ]; then
            log_error "Timeout waiting for Tilt resources after ${timeout}s"
            return 1
        fi

        # Get resource statuses
        local resources=$(get_tilt_resources)
        if [ -z "$resources" ]; then
            log_warn "Failed to get Tilt resources, retrying..."
            sleep 5
            continue
        fi

        # Parse resource statuses using jq
        # Tilt API returns uiResources with RuntimeStatus
        local total_count=$(echo "$resources" | jq -r '.uiResources | length' 2>/dev/null)
        local ready_count=$(echo "$resources" | jq -r '[.uiResources[] | select(.status.runtimeStatus == "ok" or .status.runtimeStatus == "not_applicable")] | length' 2>/dev/null)

        if [ "$total_count" = "null" ] || [ "$ready_count" = "null" ]; then
            log_warn "Unable to parse Tilt status, retrying..."
            sleep 5
            continue
        fi

        log_info "Tilt resources: $ready_count/$total_count ready (${elapsed}s elapsed)"

        # Check if all resources are ready
        if [ "$ready_count" = "$total_count" ] && [ "$total_count" -gt 0 ]; then
            log_success "All Tilt resources are ready!"
            return 0
        fi

        # Show any error resources
        local errors=$(echo "$resources" | jq -r '.uiResources[] | select(.status.runtimeStatus == "error") | .metadata.name' 2>/dev/null)
        if [ -n "$errors" ]; then
            log_warn "Resources with errors: $errors"
        fi

        sleep 5
    done
}

# Get status of a specific Tilt resource
# Usage: get_resource_status <resource_name>
get_resource_status() {
    local resource_name=$1
    local resources=$(get_tilt_resources)

    echo "$resources" | jq -r ".uiResources[] | select(.metadata.name == \"$resource_name\") | .status.runtimeStatus" 2>/dev/null
}

# List all Tilt resources with their status
# Usage: list_tilt_resources
list_tilt_resources() {
    local resources=$(get_tilt_resources)

    if [ -z "$resources" ]; then
        log_error "Failed to get Tilt resources"
        return 1
    fi

    echo "$resources" | jq -r '.uiResources[] | "\(.metadata.name): \(.status.runtimeStatus)"' 2>/dev/null | while read -r line; do
        local name=$(echo "$line" | cut -d: -f1)
        local status=$(echo "$line" | cut -d: -f2- | xargs)

        case "$status" in
        ok)
            echo -e "  ${COLOR_GREEN}✓${COLOR_RESET} $name"
            ;;
        pending | building)
            echo -e "  ${COLOR_YELLOW}⋯${COLOR_RESET} $name ($status)"
            ;;
        error)
            echo -e "  ${COLOR_RED}✗${COLOR_RESET} $name (ERROR)"
            ;;
        not_applicable)
            echo -e "  ${COLOR_BLUE}-${COLOR_RESET} $name (n/a)"
            ;;
        *)
            echo -e "  ${COLOR_YELLOW}?${COLOR_RESET} $name ($status)"
            ;;
        esac
    done
}
