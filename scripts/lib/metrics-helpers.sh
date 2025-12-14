#!/usr/bin/env bash
#
# Prometheus metrics helper functions
#

# Source common functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/common.sh"

# Prometheus endpoint
PROMETHEUS_URL="${PROMETHEUS_URL:-http://localhost:9090}"

# Query a Prometheus metric
# Usage: query_metric <metric_name>
query_metric() {
    local metric_name=$1
    curl -s "${PROMETHEUS_URL}/metrics" 2>/dev/null | grep "^${metric_name}"
}

# Get metric value (first value found)
# Usage: get_metric_value <metric_name>
get_metric_value() {
    local metric_name=$1
    query_metric "$metric_name" | head -1 | awk '{print $2}'
}

# Get metric value with label filtering
# Usage: get_metric_labels <metric_name> <label_filter>
# Example: get_metric_labels "ha_miner_claims_submitted_total" 'supplier="pokt1abc"'
get_metric_labels() {
    local metric_name=$1
    local label_filter=$2

    query_metric "$metric_name" | grep "{.*${label_filter}.*}" | awk '{print $2}'
}

# Get all values for a metric
# Usage: get_all_metric_values <metric_name>
get_all_metric_values() {
    local metric_name=$1
    query_metric "$metric_name" | awk '{print $2}'
}

# Check if metric exists
# Usage: metric_exists <metric_name>
metric_exists() {
    local metric_name=$1
    [ -n "$(query_metric "$metric_name")" ]
}

# Get metric count (sum of all values)
# Usage: get_metric_sum <metric_name>
get_metric_sum() {
    local metric_name=$1
    get_all_metric_values "$metric_name" | awk '{sum += $1} END {print sum}'
}

# Get claims submitted count for supplier
# Usage: get_claims_submitted <supplier_address>
get_claims_submitted() {
    local supplier=$1
    get_metric_labels "ha_miner_claims_submitted_total" "supplier=\"$supplier\"" | head -1
}

# Get proofs submitted count for supplier
# Usage: get_proofs_submitted <supplier_address>
get_proofs_submitted() {
    local supplier=$1
    get_metric_labels "ha_miner_proofs_submitted_total" "supplier=\"$supplier\"" | head -1
}

# Get sessions by state for supplier
# Usage: get_sessions_by_state <supplier_address> <state>
get_sessions_by_state() {
    local supplier=$1
    local state=$2
    get_metric_labels "ha_miner_sessions_by_state" "supplier=\"$supplier\",state=\"$state\"" | head -1
}

# Get relayer requests total
# Usage: get_relayer_requests_total
get_relayer_requests_total() {
    get_metric_sum "ha_relayer_requests_total"
}

# Wait for metric to reach a value
# Usage: wait_for_metric <metric_name> <expected_value> <timeout_seconds>
wait_for_metric() {
    local metric_name=$1
    local expected_value=$2
    local timeout=${3:-60}
    local elapsed=0

    log_info "Waiting for $metric_name to reach $expected_value..."

    while true; do
        local current_value
        current_value=$(get_metric_sum "$metric_name" 2>/dev/null || echo "0")

        if [ -z "$current_value" ] || [ "$current_value" = "0" ]; then
            current_value=0
        fi

        if [ "$current_value" -ge "$expected_value" ]; then
            log_success "$metric_name reached $current_value (>= $expected_value)"
            return 0
        fi

        if [ $elapsed -ge $timeout ]; then
            log_error "Timeout waiting for $metric_name (current: $current_value, expected: $expected_value)"
            return 1
        fi

        sleep 2
        ((elapsed += 2))
    done
}

# Pretty print metric with labels
# Usage: print_metric <metric_name>
print_metric() {
    local metric_name=$1
    local metrics
    metrics=$(query_metric "$metric_name")

    if [ -z "$metrics" ]; then
        echo "  $metric_name: (not found)"
        return
    fi

    echo "$metrics" | while read -r line; do
        echo "  $line"
    done
}
