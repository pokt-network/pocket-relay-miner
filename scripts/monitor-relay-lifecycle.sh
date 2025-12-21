#!/bin/bash
# Monitor relay lifecycle across all suppliers
# Usage: ./scripts/monitor-relay-lifecycle.sh [--watch]

set -e

REDIS_URL="${REDIS_URL:-redis://localhost:6379}"
WATCH_MODE="${1:-}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Get current block height
get_block_height() {
    curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height' 2>/dev/null || echo "?"
}

# Calculate session info
get_session_info() {
    local height=$1
    local session_start=$((height / 10 * 10))
    local session_end=$((session_start + 9))
    local claim_open=$((session_end + 1))
    local claim_close=$((claim_open + 4))
    local proof_open=$claim_close
    local proof_close=$((proof_open + 4))
    echo "$session_start $session_end $claim_open $claim_close $proof_open $proof_close"
}

# Get all claimed suppliers
get_claimed_suppliers() {
    kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli KEYS "ha:miner:claim:*" 2>/dev/null | sed 's/ha:miner:claim://' | tr -d '\r'
}

# Get stream info for a supplier
get_stream_info() {
    local supplier=$1
    local stream_key="ha:relays:$supplier"

    # Get stream length
    local length=$(kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli XLEN "$stream_key" 2>/dev/null | tr -d '\r')

    # Get pending count from consumer group
    local pending=$(kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli XINFO GROUPS "$stream_key" 2>/dev/null | grep -A1 "pending" | tail -1 | tr -d '\r' 2>/dev/null || echo "0")

    echo "${length:-0} ${pending:-0}"
}

# Get session count and relay totals for a supplier
get_session_summary() {
    local supplier=$1
    local index_key="ha:miner:sessions:$supplier:index"

    # Get session count
    local count=$(kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli SCARD "$index_key" 2>/dev/null | tr -d '\r')

    # Get all sessions and sum relays
    local sessions=$(kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli SMEMBERS "$index_key" 2>/dev/null | tr -d '\r')
    local total_relays=0
    local states=""

    for session in $sessions; do
        local session_key="ha:miner:sessions:$supplier:$session"
        local data=$(kubectl exec -it redis-standalone-0 -c redis-standalone -- redis-cli GET "$session_key" 2>/dev/null)
        if [ -n "$data" ]; then
            local relays=$(echo "$data" | jq -r '.relay_count // 0' 2>/dev/null)
            local state=$(echo "$data" | jq -r '.state // "unknown"' 2>/dev/null)
            total_relays=$((total_relays + relays))
            states="$states $state:$relays"
        fi
    done

    echo "${count:-0} $total_relays $states"
}

# Main monitoring function
monitor() {
    clear
    local height=$(get_block_height)
    local session_info=($(get_session_info $height))

    echo -e "${BLUE}═══════════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}                    RELAY LIFECYCLE MONITOR                         ${NC}"
    echo -e "${BLUE}═══════════════════════════════════════════════════════════════════${NC}"
    echo ""
    echo -e "Block Height: ${GREEN}$height${NC}"
    echo -e "Current Session: ${session_info[0]}-${session_info[1]}"
    echo -e "Claim Window: ${session_info[2]}-${session_info[3]}"
    echo -e "Proof Window: ${session_info[4]}-${session_info[5]}"
    echo ""

    # Determine current phase
    if [ "$height" -le "${session_info[1]}" ]; then
        echo -e "Phase: ${GREEN}ACTIVE SESSION${NC} (accepting relays)"
    elif [ "$height" -lt "${session_info[2]}" ]; then
        echo -e "Phase: ${YELLOW}GRACE PERIOD${NC} (session ended, claim window pending)"
    elif [ "$height" -le "${session_info[3]}" ]; then
        echo -e "Phase: ${YELLOW}CLAIM WINDOW${NC} (claims being submitted)"
    elif [ "$height" -le "${session_info[5]}" ]; then
        echo -e "Phase: ${BLUE}PROOF WINDOW${NC} (proofs being submitted)"
    else
        echo -e "Phase: ${GREEN}COMPLETE${NC}"
    fi
    echo ""

    echo -e "${BLUE}───────────────────────────────────────────────────────────────────${NC}"
    echo -e "SUPPLIER STREAMS & SESSIONS"
    echo -e "${BLUE}───────────────────────────────────────────────────────────────────${NC}"
    printf "%-45s %8s %8s %8s %s\n" "SUPPLIER" "STREAM" "PENDING" "SESSIONS" "RELAYS (by state)"
    echo "─────────────────────────────────────────────────────────────────────────────────"

    local total_stream=0
    local total_pending=0
    local total_sessions=0
    local total_relays=0

    for supplier in $(get_claimed_suppliers); do
        local stream_info=($(get_stream_info $supplier))
        local session_info=($(get_session_summary $supplier))

        local stream_len=${stream_info[0]:-0}
        local pending=${stream_info[1]:-0}
        local session_count=${session_info[0]:-0}
        local relay_total=${session_info[1]:-0}
        local states="${session_info[@]:2}"

        total_stream=$((total_stream + stream_len))
        total_pending=$((total_pending + pending))
        total_sessions=$((total_sessions + session_count))
        total_relays=$((total_relays + relay_total))

        # Color pending based on value
        local pending_color=$GREEN
        [ "$pending" -gt 0 ] && pending_color=$YELLOW
        [ "$pending" -gt 100 ] && pending_color=$RED

        printf "%-45s %8d ${pending_color}%8d${NC} %8d %s\n" \
            "${supplier:0:45}" "$stream_len" "$pending" "$session_count" "$states"
    done

    echo "─────────────────────────────────────────────────────────────────────────────────"
    printf "%-45s %8d %8d %8d %d total\n" "TOTAL" "$total_stream" "$total_pending" "$total_sessions" "$total_relays"
    echo ""

    if [ "$WATCH_MODE" = "--watch" ]; then
        echo -e "${YELLOW}Refreshing in 5 seconds... (Ctrl+C to stop)${NC}"
        sleep 5
        monitor
    fi
}

# Run
monitor
