#!/bin/bash
# Profile CPU and memory during relay load test
# Usage: ./scripts/profile-load-test.sh [relay_count] [concurrency]
#
# Prerequisites:
#   - Tilt running with 1 miner, 1 relayer
#   - pprof ports forwarded: relayer=6060, miner=6065
#   - go tool pprof available

set -e

RELAY_COUNT=${1:-3000}
CONCURRENCY=${2:-50}
PROFILE_DURATION=30  # seconds for CPU profile

# pprof endpoints (via kubectl port-forward)
RELAYER_PPROF="http://localhost:6060"
MINER_PPROF="http://localhost:6065"

# Output directory
TIMESTAMP=$(date +%Y%m%d_%H%M%S)
OUTPUT_DIR="profiles/${TIMESTAMP}"
mkdir -p "$OUTPUT_DIR"

echo "=============================================="
echo "       RELAY LOAD TEST WITH PROFILING        "
echo "=============================================="
echo "Relays:      $RELAY_COUNT"
echo "Concurrency: $CONCURRENCY"
echo "Output:      $OUTPUT_DIR"
echo "=============================================="
echo ""

# Check pprof endpoints
echo "Checking pprof endpoints..."
if ! curl -s "${RELAYER_PPROF}/debug/pprof/" > /dev/null 2>&1; then
    echo "ERROR: Relayer pprof not accessible at ${RELAYER_PPROF}"
    echo "Run: kubectl port-forward svc/relayer 6060:6060"
    exit 1
fi

if ! curl -s "${MINER_PPROF}/debug/pprof/" > /dev/null 2>&1; then
    echo "ERROR: Miner pprof not accessible at ${MINER_PPROF}"
    echo "Run: kubectl port-forward svc/miner 6065:6065"
    exit 1
fi
echo "Both pprof endpoints accessible"
echo ""

# Get baseline memory/goroutine profiles
echo "Collecting baseline profiles..."
curl -s "${RELAYER_PPROF}/debug/pprof/heap" > "${OUTPUT_DIR}/relayer_heap_before.pb.gz"
curl -s "${MINER_PPROF}/debug/pprof/heap" > "${OUTPUT_DIR}/miner_heap_before.pb.gz"
curl -s "${RELAYER_PPROF}/debug/pprof/goroutine" > "${OUTPUT_DIR}/relayer_goroutine_before.pb.gz"
curl -s "${MINER_PPROF}/debug/pprof/goroutine" > "${OUTPUT_DIR}/miner_goroutine_before.pb.gz"
echo "Baseline profiles saved"
echo ""

# Start CPU profiling in background (captures during load)
echo "Starting CPU profiling (${PROFILE_DURATION}s)..."
curl -s "${RELAYER_PPROF}/debug/pprof/profile?seconds=${PROFILE_DURATION}" > "${OUTPUT_DIR}/relayer_cpu.pb.gz" &
RELAYER_CPU_PID=$!
curl -s "${MINER_PPROF}/debug/pprof/profile?seconds=${PROFILE_DURATION}" > "${OUTPUT_DIR}/miner_cpu.pb.gz" &
MINER_CPU_PID=$!

# Give profiler a moment to start
sleep 2

# Run load test
echo ""
echo "Running load test: ${RELAY_COUNT} relays @ ${CONCURRENCY} concurrency..."
echo ""

LOAD_START=$(date +%s)
hey -n "$RELAY_COUNT" -c "$CONCURRENCY" \
    -H "Target-Service-Id: develop-http" \
    -H "Content-Type: application/json" \
    -m POST \
    -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
    http://localhost:3069/v1 2>&1 | tee "${OUTPUT_DIR}/load_test_results.txt"
LOAD_END=$(date +%s)
LOAD_DURATION=$((LOAD_END - LOAD_START))

echo ""
echo "Load test completed in ${LOAD_DURATION}s"
echo ""

# Wait for CPU profiling to complete
echo "Waiting for CPU profiles to complete..."
wait $RELAYER_CPU_PID 2>/dev/null || true
wait $MINER_CPU_PID 2>/dev/null || true
echo "CPU profiles collected"
echo ""

# Get post-load memory/goroutine profiles
echo "Collecting post-load profiles..."
curl -s "${RELAYER_PPROF}/debug/pprof/heap" > "${OUTPUT_DIR}/relayer_heap_after.pb.gz"
curl -s "${MINER_PPROF}/debug/pprof/heap" > "${OUTPUT_DIR}/miner_heap_after.pb.gz"
curl -s "${RELAYER_PPROF}/debug/pprof/goroutine" > "${OUTPUT_DIR}/relayer_goroutine_after.pb.gz"
curl -s "${MINER_PPROF}/debug/pprof/goroutine" > "${OUTPUT_DIR}/miner_goroutine_after.pb.gz"

# Get allocs profile (shows allocations over time)
curl -s "${RELAYER_PPROF}/debug/pprof/allocs" > "${OUTPUT_DIR}/relayer_allocs.pb.gz"
curl -s "${MINER_PPROF}/debug/pprof/allocs" > "${OUTPUT_DIR}/miner_allocs.pb.gz"

echo "Post-load profiles saved"
echo ""

# Generate summary
echo "=============================================="
echo "               PROFILING COMPLETE             "
echo "=============================================="
echo ""
echo "Profiles saved to: $OUTPUT_DIR"
echo ""
echo "Files:"
ls -la "$OUTPUT_DIR"
echo ""
echo "=============================================="
echo "            QUICK ANALYSIS COMMANDS           "
echo "=============================================="
echo ""
echo "# View CPU hotspots (top functions):"
echo "go tool pprof -top ${OUTPUT_DIR}/relayer_cpu.pb.gz"
echo "go tool pprof -top ${OUTPUT_DIR}/miner_cpu.pb.gz"
echo ""
echo "# Interactive CPU analysis:"
echo "go tool pprof -http=:8080 ${OUTPUT_DIR}/relayer_cpu.pb.gz"
echo "go tool pprof -http=:8081 ${OUTPUT_DIR}/miner_cpu.pb.gz"
echo ""
echo "# Memory analysis (heap):"
echo "go tool pprof -top ${OUTPUT_DIR}/relayer_heap_after.pb.gz"
echo "go tool pprof -top ${OUTPUT_DIR}/miner_heap_after.pb.gz"
echo ""
echo "# Allocation analysis (look for ring recreation):"
echo "go tool pprof -alloc_objects -top ${OUTPUT_DIR}/relayer_allocs.pb.gz | head -30"
echo ""
echo "# Search for ring-related allocations:"
echo "go tool pprof -alloc_objects -top ${OUTPUT_DIR}/relayer_allocs.pb.gz 2>/dev/null | grep -i ring"
echo ""
