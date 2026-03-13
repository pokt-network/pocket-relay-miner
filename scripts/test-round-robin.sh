#!/bin/bash
# test-round-robin.sh - Verify round-robin distribution across backends
# Sends N relays to the multi-backend HTTP service and reports per-backend distribution.
# Requires: Tilt environment running, jq installed
# Usage: ./scripts/test-round-robin.sh [count]
#   count: number of relays to send (default: 1000)

set -euo pipefail

TOTAL=${1:-1000}
GATEWAY="http://localhost:3069/v1"

# Check dependencies
if ! command -v jq &>/dev/null; then
    echo "ERROR: jq is required but not installed."
    exit 1
fi

# Pre-flight check
echo "Pre-flight: checking gateway at $GATEWAY ..."
status=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Target-Service-Id: develop-http" \
    -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":0}' \
    "$GATEWAY" 2>/dev/null || echo "000")

if [ "$status" != "200" ]; then
    echo "ERROR: Gateway returned HTTP $status (expected 200). Is Tilt running?"
    exit 1
fi
echo "Pre-flight: OK (HTTP $status)"
echo ""

# Send relays and collect backend_id from each response
declare -A counts
errors=0

echo "Sending $TOTAL relays..."
for i in $(seq 1 "$TOTAL"); do
    resp=$(curl -s -X POST -H "Content-Type: application/json" \
        -H "Target-Service-Id: develop-http" \
        -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":'"$i"'}' \
        "$GATEWAY")
    backend_id=$(echo "$resp" | jq -r '.result.backend_id // "unknown"')
    if [ -z "$backend_id" ] || [ "$backend_id" = "null" ] || [ "$backend_id" = "unknown" ]; then
        errors=$((errors + 1))
    else
        if [[ -v "counts[$backend_id]" ]]; then
            counts["$backend_id"]=$(( counts["$backend_id"] + 1 ))
        else
            counts["$backend_id"]=1
        fi
    fi
    # Print progress every 100 relays
    if [ $((i % 100)) -eq 0 ]; then
        echo -ne "\rProgress: $i/$TOTAL"
    fi
done
echo -e "\r                        "

# Print distribution report
echo "=== Round-Robin Distribution Report ==="
echo "Total relays sent: $TOTAL"
echo "Errors/unknown: $errors"
echo ""
echo "Per-backend distribution:"
for backend in $(echo "${!counts[@]}" | tr ' ' '\n' | sort); do
    count=${counts["$backend"]}
    pct=$(echo "scale=1; $count * 100 / $TOTAL" | bc)
    bar_len=$(echo "$count * 40 / $TOTAL" | bc)
    bar=$(printf '%*s' "$bar_len" '' | tr ' ' '#')
    echo "  $backend: $count ($pct%) $bar"
done
echo ""
echo "Done."
