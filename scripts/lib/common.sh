#!/usr/bin/env bash
#
# Common utility functions for integration testing scripts
#

# Colors for output
COLOR_RED='\033[0;31m'
COLOR_GREEN='\033[0;32m'
COLOR_YELLOW='\033[0;33m'
COLOR_BLUE='\033[0;34m'
COLOR_RESET='\033[0m'

# Logging functions with timestamps
# All log functions write to stderr to avoid polluting stdout when capturing command output
log_info() {
    echo -e "${COLOR_BLUE}[$(date +'%H:%M:%S')]${COLOR_RESET} $*" >&2
}

log_success() {
    echo -e "${COLOR_GREEN}[$(date +'%H:%M:%S')] ✅${COLOR_RESET} $*" >&2
}

log_error() {
    echo -e "${COLOR_RED}[$(date +'%H:%M:%S')] ❌${COLOR_RESET} $*" >&2
}

log_warn() {
    echo -e "${COLOR_YELLOW}[$(date +'%H:%M:%S')] ⚠️${COLOR_RESET} $*" >&2
}

# Wait for a TCP port to be open
# Usage: wait_for_port <host> <port> <timeout_seconds>
wait_for_port() {
    local host=$1
    local port=$2
    local timeout=${3:-30}
    local elapsed=0

    log_info "Waiting for $host:$port to be ready..."

    while ! nc -z "$host" "$port" 2>/dev/null; do
        if [ $elapsed -ge $timeout ]; then
            log_error "Timeout waiting for $host:$port after ${timeout}s"
            return 1
        fi
        sleep 1
        ((elapsed++))
    done

    log_success "$host:$port is ready"
    return 0
}

# Parse JSON using jq
# Usage: parse_json <json_string> <jq_filter>
parse_json() {
    local json=$1
    local filter=$2

    if ! command -v jq &>/dev/null; then
        log_error "jq is not installed. Please install jq to parse JSON."
        return 1
    fi

    echo "$json" | jq -r "$filter" 2>/dev/null
}

# Format milliseconds to human-readable duration
# Usage: format_duration <milliseconds>
format_duration() {
    local ms=$1

    # Handle empty or non-numeric input
    if [ -z "$ms" ] || ! [[ "$ms" =~ ^[0-9]+$ ]]; then
        echo "0ms"
        return
    fi

    if [ "$ms" -lt 1000 ]; then
        echo "${ms}ms"
    elif [ "$ms" -lt 60000 ]; then
        local seconds=$((ms / 1000))
        local remaining_ms=$((ms % 1000))
        printf "%d.%03ds" "$seconds" "$remaining_ms"
    else
        local minutes=$((ms / 60000))
        local remaining_ms=$((ms % 60000))
        local seconds=$((remaining_ms / 1000))
        printf "%dm %ds" "$minutes" "$seconds"
    fi
}

# Check if a command exists
# Usage: command_exists <command>
command_exists() {
    command -v "$1" &>/dev/null
}

# Ensure required commands are available
# Usage: require_commands cmd1 cmd2 cmd3
require_commands() {
    local missing=()

    for cmd in "$@"; do
        if ! command_exists "$cmd"; then
            missing+=("$cmd")
        fi
    done

    if [ ${#missing[@]} -gt 0 ]; then
        log_error "Missing required commands: ${missing[*]}"
        log_error "Please install them and try again."
        return 1
    fi

    return 0
}

# Find pocket-relay-miner binary
# Usage: MINER_BIN=$(find_miner_binary)
find_miner_binary() {
    # Try PATH first
    if command_exists pocket-relay-miner; then
        echo "pocket-relay-miner"
        return 0
    fi

    # Try ./bin/pocket-relay-miner
    if [ -x "./bin/pocket-relay-miner" ]; then
        echo "./bin/pocket-relay-miner"
        return 0
    fi

    # Try ../bin/pocket-relay-miner (from scripts directory)
    if [ -x "../bin/pocket-relay-miner" ]; then
        echo "../bin/pocket-relay-miner"
        return 0
    fi

    log_error "pocket-relay-miner binary not found in PATH or ./bin/"
    log_error "Please run 'make build' first"
    return 1
}

# Print a separator line
print_separator() {
    echo "────────────────────────────────────────────────────────────────"
}

# Print a header
print_header() {
    echo
    print_separator
    echo "$*"
    print_separator
}

# Get current timestamp in seconds
timestamp_now() {
    date +%s
}

# Calculate elapsed time
# Usage: elapsed=$(elapsed_time $start_time)
elapsed_time() {
    local start=$1
    local now=$(timestamp_now)
    echo $((now - start))
}
