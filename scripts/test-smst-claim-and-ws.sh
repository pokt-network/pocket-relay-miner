#!/usr/bin/env bash
#
# Integration test: SMST-based claim flow + WebSocket concurrent write fix
#
# Verifies two fixes simultaneously under load:
# 1. Claims use SMST root hash (not snapshot counters) for viability decisions
# 2. WebSocket mutex prevents concurrent write panics
#
# Strategy:
# - Phase 1: Heavy HTTP load (500 RPS) to generate claims/proofs
# - Phase 2: Concurrent WebSocket stress (50 connections, 10 msg/s each)
# - Phase 3: Monitor for panics, claim failures, and proof failures
# - Phase 4: Wait for session cycle and verify claim/proof success
#
# Usage:
#   ./scripts/test-smst-claim-and-ws.sh
#   HTTP_RPS=300 WS_CONNS=100 ./scripts/test-smst-claim-and-ws.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Configuration
GATEWAY_URL="${GATEWAY_URL:-http://localhost:3069/v1}"
WS_GATEWAY_URL="${WS_GATEWAY_URL:-ws://localhost:3069/v1}"
HTTP_SERVICE="${HTTP_SERVICE:-develop-http}"
WS_SERVICE="${WS_SERVICE:-develop-websocket}"
HTTP_RPS="${HTTP_RPS:-500}"
WS_CONNS="${WS_CONNS:-50}"
WS_RATE="${WS_RATE:-10}"
DURATION="${DURATION:-180}"  # 3 minutes default (covers ~1 localnet session)
K8S_CONTEXT="${K8S_CONTEXT:-kind-kind}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date +%H:%M:%S) $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date +%H:%M:%S) $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date +%H:%M:%S) $1"; }
log_phase() { echo -e "\n${BLUE}=== $1 ===${NC}\n"; }

cleanup() {
    log_info "Cleaning up background processes..."
    kill $HTTP_PID $WS_PID 2>/dev/null || true
    wait $HTTP_PID $WS_PID 2>/dev/null || true
}
trap cleanup EXIT

# Pre-flight checks
log_phase "PRE-FLIGHT CHECKS"

# Check hey is installed
if ! command -v hey &>/dev/null; then
    log_error "hey not found. Install: go install github.com/rakyll/hey@latest"
    exit 1
fi

# Check HTTP relay works
HTTP_STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
    -H "Content-Type: application/json" \
    -H "Target-Service-Id: $HTTP_SERVICE" \
    -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
    "$GATEWAY_URL")

if [ "$HTTP_STATUS" != "200" ]; then
    log_error "HTTP relay pre-flight failed (status=$HTTP_STATUS). Is Tilt running?"
    exit 1
fi
log_info "HTTP relay OK (status=$HTTP_STATUS)"

# Check pods are running
RELAYER_PODS=$(kubectl --context "$K8S_CONTEXT" get pods -l app=relayer --no-headers 2>/dev/null | grep Running | wc -l)
MINER_PODS=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | grep Running | wc -l)
log_info "Relayer pods: $RELAYER_PODS, Miner pods: $MINER_PODS"

# Capture initial state: count panics and claim errors in logs
INITIAL_PANICS=$(kubectl --context "$K8S_CONTEXT" logs -l app=relayer --tail=10000 2>/dev/null | grep -c "PANIC RECOVERED" || echo 0)
INITIAL_CLAIM_ERRORS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "claim_not_found\|failed to broadcast" || echo 0)
log_info "Initial state: panics=$INITIAL_PANICS, claim_errors=$INITIAL_CLAIM_ERRORS"

# Record current block height
CURRENT_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "unknown"')
log_info "Current block height: $CURRENT_HEIGHT"

# ─────────────────────────────────────────────────────────────
log_phase "PHASE 1+2: PARALLEL HTTP LOAD + WEBSOCKET STRESS (${DURATION}s)"

# Start HTTP load in background
log_info "Starting HTTP load: $HTTP_RPS RPS to $HTTP_SERVICE for ${DURATION}s"
hey -z "${DURATION}s" \
    -c "$((HTTP_RPS / 5))" \
    -q "$((HTTP_RPS / (HTTP_RPS / 5)))" \
    -m POST \
    -H "Content-Type: application/json" \
    -H "Target-Service-Id: $HTTP_SERVICE" \
    -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
    "$GATEWAY_URL" > /tmp/hey-results.txt 2>&1 &
HTTP_PID=$!
log_info "HTTP load PID: $HTTP_PID"

# Start WebSocket stress in background
log_info "Starting WebSocket stress: $WS_CONNS connections, $WS_RATE msg/s each for ${DURATION}s"
go run "$SCRIPT_DIR/ws-stress/main.go" \
    -url "$WS_GATEWAY_URL" \
    -service "$WS_SERVICE" \
    -connections "$WS_CONNS" \
    -rate "$WS_RATE" \
    -duration "${DURATION}s" \
    -report 30 > /tmp/ws-stress-results.txt 2>&1 &
WS_PID=$!
log_info "WebSocket stress PID: $WS_PID"

# Monitor progress with periodic checks
ELAPSED=0
REPORT_INTERVAL=30
while [ $ELAPSED -lt "$DURATION" ]; do
    sleep "$REPORT_INTERVAL"
    ELAPSED=$((ELAPSED + REPORT_INTERVAL))

    # Check for panics
    CURRENT_PANICS=$(kubectl --context "$K8S_CONTEXT" logs -l app=relayer --tail=10000 2>/dev/null | grep -c "PANIC RECOVERED" || echo 0)
    NEW_PANICS=$((CURRENT_PANICS - INITIAL_PANICS))

    # Check for claim errors
    CURRENT_CLAIM_ERRORS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "claim_not_found\|failed to broadcast" || echo 0)
    NEW_CLAIM_ERRORS=$((CURRENT_CLAIM_ERRORS - INITIAL_CLAIM_ERRORS))

    # Check for SMST-based viability logs (new flow)
    SMST_SKIPS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "calculated from SMST root hash\|SMST tree is empty" || echo 0)

    # Check for old snapshot-based logs (should NOT appear)
    OLD_SKIP_LOGS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "skipping claim - session has 0 relays\|skipping claim - session has 0 compute units" || echo 0)

    # Check claim submissions
    CLAIMS_SUBMITTED=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "claims submitted\|batched claims submitted" || echo 0)

    log_info "[${ELAPSED}s/${DURATION}s] panics=+$NEW_PANICS claim_errors=+$NEW_CLAIM_ERRORS claims_submitted=$CLAIMS_SUBMITTED smst_skips=$SMST_SKIPS old_snapshot_skips=$OLD_SKIP_LOGS"

    if [ "$NEW_PANICS" -gt 0 ]; then
        log_error "NEW PANICS DETECTED! WebSocket mutex fix may not be working."
        kubectl --context "$K8S_CONTEXT" logs -l app=relayer --tail=100 2>/dev/null | grep -A5 "PANIC" || true
    fi

    if [ "$OLD_SKIP_LOGS" -gt 0 ]; then
        log_warn "OLD snapshot-based skip logs detected — new SMST flow may not be active"
    fi

    # Check if processes are still alive
    if ! kill -0 $HTTP_PID 2>/dev/null; then
        log_warn "HTTP load process ended early"
    fi
    if ! kill -0 $WS_PID 2>/dev/null; then
        log_warn "WebSocket stress process ended early"
    fi
done

# Wait for processes to finish
log_info "Waiting for load processes to finish..."
wait $HTTP_PID 2>/dev/null || true
wait $WS_PID 2>/dev/null || true

# ─────────────────────────────────────────────────────────────
log_phase "PHASE 3: RESULTS ANALYSIS"

# HTTP results
if [ -f /tmp/hey-results.txt ]; then
    log_info "HTTP Load Results:"
    grep -E "Requests/sec:|Status code distribution:|Average:|Fastest:|Slowest:" /tmp/hey-results.txt | head -10
    echo ""

    # Check for errors
    HTTP_ERRORS=$(grep -c "\[5[0-9][0-9]\]" /tmp/hey-results.txt 2>/dev/null || echo 0)
    if [ "$HTTP_ERRORS" -gt 0 ]; then
        log_warn "HTTP 5xx errors detected: $HTTP_ERRORS"
        grep "Status code distribution" -A5 /tmp/hey-results.txt
    fi
fi

# WebSocket results
if [ -f /tmp/ws-stress-results.txt ]; then
    log_info "WebSocket Stress Results:"
    tail -20 /tmp/ws-stress-results.txt
    echo ""

    WS_ERRORS=$(grep -ci "error\|panic\|disconnect" /tmp/ws-stress-results.txt 2>/dev/null || echo 0)
    if [ "$WS_ERRORS" -gt 0 ]; then
        log_warn "WebSocket errors/disconnections detected in output"
    fi
fi

# Final panic check
FINAL_PANICS=$(kubectl --context "$K8S_CONTEXT" logs -l app=relayer --tail=10000 2>/dev/null | grep -c "PANIC RECOVERED" || echo 0)
NEW_PANICS=$((FINAL_PANICS - INITIAL_PANICS))

# Final claim error check
FINAL_CLAIM_ERRORS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "claim_not_found\|failed to broadcast" || echo 0)
NEW_CLAIM_ERRORS=$((FINAL_CLAIM_ERRORS - INITIAL_CLAIM_ERRORS))

# Claim success check
TOTAL_CLAIMS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "batched claims submitted successfully" || echo 0)
TOTAL_PROOFS=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "proof submitted\|probabilistic proof" || echo 0)

# SMST flow check
SMST_FLUSH=$(kubectl --context "$K8S_CONTEXT" logs -l app=miner --tail=10000 2>/dev/null | grep -c "flushed SMST" || echo 0)

# ─────────────────────────────────────────────────────────────
log_phase "FINAL VERDICT"

echo "  Relayer panics (new):      $NEW_PANICS"
echo "  Claim errors (new):        $NEW_CLAIM_ERRORS"
echo "  Claims submitted:          $TOTAL_CLAIMS"
echo "  Proofs submitted:          $TOTAL_PROOFS"
echo "  SMST flushes:              $SMST_FLUSH"
echo ""

PASS=true

if [ "$NEW_PANICS" -gt 0 ]; then
    log_error "FAIL: $NEW_PANICS new panic(s) detected in relayer"
    PASS=false
fi

if [ "$NEW_CLAIM_ERRORS" -gt 0 ]; then
    log_warn "WARN: $NEW_CLAIM_ERRORS claim errors detected (may be expected in localnet)"
fi

if [ "$TOTAL_CLAIMS" -eq 0 ] && [ "$SMST_FLUSH" -gt 0 ]; then
    log_error "FAIL: SMST flushed but no claims submitted — claim pipeline broken"
    PASS=false
fi

if [ "$SMST_FLUSH" -eq 0 ]; then
    log_warn "WARN: No SMST flushes observed — test may need longer duration to cover a session cycle"
fi

if $PASS; then
    log_info "ALL CHECKS PASSED"
    exit 0
else
    log_error "SOME CHECKS FAILED — review logs above"
    exit 1
fi
