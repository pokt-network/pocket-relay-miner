#!/usr/bin/env bash
#
# Maximum stress test: claims/proofs + WebSocket concurrency
#
# Prerequisites:
#   - Genesis with CUTTM=100, granularity=1
#   - tilt down && tilt up (fresh cluster with new genesis)
#   - hey installed: go install github.com/rakyll/hey@latest
#
# Expected math (base difficulty, no relay mining difficulty configured):
#   compute_units_per_relay = 1,000
#   CUTTM = 100, granularity = 1
#   difficulty_multiplier ≈ 1.0 (base, all 0xFF)
#
#   reward_upokt = num_relays * 1,000 * 1.0 * 100 / 1
#                = num_relays * 100,000
#
#   proof_fee = 1,000,000 uPOKT (1 POKT)
#   breakeven = 10 relays (reward = 1,000,000 = fee)
#   profitable at 11+ relays
#
# With 500+ relays per session: reward = 50,000,000+ uPOKT >> fee
# Claims MUST be submitted and proofs MUST succeed.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ─── Configuration ───────────────────────────────────────────
GATEWAY_URL="${GATEWAY_URL:-http://localhost:3069/v1}"
WS_GATEWAY_URL="${WS_GATEWAY_URL:-ws://localhost:3069/v1}"
HTTP_SERVICE="develop-http"
WS_SERVICE="develop-websocket"
K8S_CONTEXT="kind-kind"

# Stress levels
HTTP_CONCURRENCY=200          # concurrent HTTP connections
HTTP_RPS_PER_CONN=5           # requests per second per connection (total: 1000 RPS)
WS_CONNECTIONS=200            # concurrent WebSocket connections
WS_MSG_RATE=20                # messages per second per WS connection (total: 4000 msg/s)
DURATION=300                  # 5 minutes — covers ~3 session cycles (10 blocks each)
REPORT_INTERVAL=15            # report every 15 seconds
LOKI_URL="${LOKI_URL:-http://localhost:3100}"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; BOLD='\033[1m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date +%H:%M:%S) $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date +%H:%M:%S) $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date +%H:%M:%S) $1"; }
log_phase() { echo -e "\n${BLUE}${BOLD}═══ $1 ═══${NC}\n"; }

# Loki query helper — returns count of matching log lines since $LOKI_START_NS
# Usage: loki_count "app_label" "logql_filter"
loki_count() {
    local app="$1"
    local filter="$2"
    local now_ns
    now_ns=$(date +%s)000000000
    curl -sG "${LOKI_URL}/loki/api/v1/query_range" \
        --data-urlencode "query={app=\"${app}\"} ${filter}" \
        --data-urlencode "limit=5000" \
        --data-urlencode "start=${LOKI_START_NS}" \
        --data-urlencode "end=${now_ns}" 2>/dev/null | \
        jq -r '[.data.result[].values | length] | add // 0' 2>/dev/null || echo "0"
}

# Loki query helper — returns raw log lines
# Usage: loki_lines "app_label" "logql_filter" limit
loki_lines() {
    local app="$1"
    local filter="$2"
    local limit="${3:-20}"
    local now_ns
    now_ns=$(date +%s)000000000
    curl -sG "${LOKI_URL}/loki/api/v1/query_range" \
        --data-urlencode "query={app=\"${app}\"} ${filter}" \
        --data-urlencode "limit=${limit}" \
        --data-urlencode "start=${LOKI_START_NS}" \
        --data-urlencode "end=${now_ns}" 2>/dev/null | \
        jq -r '.data.result[].values[][1]' 2>/dev/null
}

PIDS=()
cleanup() {
    log_info "Shutting down..."
    for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
    wait "${PIDS[@]}" 2>/dev/null || true
}
trap cleanup EXIT

# ─── Pre-flight ──────────────────────────────────────────────
log_phase "PRE-FLIGHT"

command -v hey &>/dev/null || { log_error "hey not found. go install github.com/rakyll/hey@latest"; exit 1; }

# Wait for relayer to be ready
for i in $(seq 1 30); do
    STATUS=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
        -H "Content-Type: application/json" \
        -H "Target-Service-Id: $HTTP_SERVICE" \
        -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
        "$GATEWAY_URL" 2>/dev/null || echo "000")
    if [ "$STATUS" = "200" ]; then break; fi
    echo "  waiting for relayer ($i/30, status=$STATUS)..."
    sleep 2
done
[ "$STATUS" != "200" ] && { log_error "Relayer not ready after 60s"; exit 1; }
log_info "HTTP relay OK"

RELAYER_PODS=$(kubectl --context "$K8S_CONTEXT" get pods -l app=relayer --no-headers 2>/dev/null | grep -c Running)
MINER_PODS=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | grep -c Running)
log_info "Pods: relayer=$RELAYER_PODS miner=$MINER_PODS"

START_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "0"')
log_info "Start block height: $START_HEIGHT"

# Set Loki start timestamp (all queries from this point forward)
LOKI_START_NS=$(date +%s)000000000

# Verify Loki is reachable
if ! curl -s "${LOKI_URL}/ready" &>/dev/null; then
    log_warn "Loki not reachable at ${LOKI_URL} — falling back to kubectl logs (less reliable under chaos)"
fi

echo ""
echo -e "${BOLD}  Stress Configuration:${NC}"
echo "    HTTP: ${HTTP_CONCURRENCY} conns × ${HTTP_RPS_PER_CONN} RPS = $((HTTP_CONCURRENCY * HTTP_RPS_PER_CONN)) total RPS"
echo "    WS:   ${WS_CONNECTIONS} conns × ${WS_MSG_RATE} msg/s = $((WS_CONNECTIONS * WS_MSG_RATE)) total msg/s"
echo "    Duration: ${DURATION}s (~$((DURATION / 100)) session cycles)"
echo ""
echo -e "${BOLD}  Expected Math (per session, base difficulty):${NC}"
echo "    If ~500 relays/session/supplier: reward = 500 × 1000 × 100 = 50,000,000 uPOKT (50 POKT)"
echo "    proof_fee = 1,000,000 uPOKT (1 POKT)"
echo "    → Claims MUST be submitted (reward >> fee)"
echo ""

# ─── Phase 1: Launch Load ───────────────────────────────────
log_phase "LAUNCHING LOAD (${DURATION}s)"

# HTTP load with body verification (catches PATH's 200+empty masking)
TOTAL_RPS=$((HTTP_CONCURRENCY * HTTP_RPS_PER_CONN))
log_info "Starting HTTP: ${TOTAL_RPS} RPS with body verification"
go run "$SCRIPT_DIR/loadtest/http-verify.go" \
    -url "$GATEWAY_URL" \
    -service "$HTTP_SERVICE" \
    -rps "$TOTAL_RPS" \
    -workers "$HTTP_CONCURRENCY" \
    -duration "${DURATION}s" \
    -report "$REPORT_INTERVAL" > /tmp/stress-hey.txt 2>&1 &
PIDS+=($!)
log_info "  PID: ${PIDS[-1]}"

# WebSocket stress
log_info "Starting WS: ${WS_CONNECTIONS} conns × ${WS_MSG_RATE} msg/s"
go run "$SCRIPT_DIR/ws-stress/main.go" \
    -url "$WS_GATEWAY_URL" \
    -service "$WS_SERVICE" \
    -connections "$WS_CONNECTIONS" \
    -rate "$WS_MSG_RATE" \
    -duration "${DURATION}s" \
    -report "$REPORT_INTERVAL" > /tmp/stress-ws.txt 2>&1 &
PIDS+=($!)
log_info "  PID: ${PIDS[-1]}"

# ─── Phase 2: Monitor (via Loki — survives pod restarts) ────
log_phase "MONITORING (via Loki)"

ELAPSED=0
while [ $ELAPSED -lt "$DURATION" ]; do
    sleep "$REPORT_INTERVAL"
    ELAPSED=$((ELAPSED + REPORT_INTERVAL))

    HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')
    PANICS=$(loki_count "relayer" '|= "PANIC RECOVERED"')
    WS_RACE=$(loki_count "relayer" '|= "concurrent write"')
    FLUSHES=$(loki_count "miner" '|= "flushed SMST"')
    CLAIMS=$(loki_count "miner" '|= "batched claims submitted successfully"')
    CLAIM_BATCH=$(loki_count "miner" '|= "claim batch complete"')
    PROOF_EXEC=$(loki_count "miner" '|= "executing batched proof transition"')
    PROOF_CLOSED=$(loki_count "miner" '|= "proof_window_closed"')
    SKIPS=$(loki_count "miner" '|= "SKIP UNPROFITABLE"')

    printf "${GREEN}[%3ds/${DURATION}s]${NC} h=%-4s pan=%-2s ws=%-2s flush=%-3s claims=%-3s claim_batch=%-3s proof_exec=%-3s prf_closed=%-3s skips=%-3s\n" \
        "$ELAPSED" "$HEIGHT" "$PANICS" "$WS_RACE" "$FLUSHES" "$CLAIMS" "$CLAIM_BATCH" "$PROOF_EXEC" "$PROOF_CLOSED" "$SKIPS"

    if [ "$PANICS" -gt 0 ]; then
        log_error "PANIC DETECTED:"
        loki_lines "relayer" '|= "PANIC"' 5
    fi
done

# Wait for load to finish
log_info "Waiting for load processes..."
for pid in "${PIDS[@]}"; do wait "$pid" 2>/dev/null || true; done
PIDS=()

# ─── Phase 3: Results (all from Loki) ─────────────────────
log_phase "RESULTS"

# HTTP with body verification
echo -e "${BOLD}─── HTTP Load (body-verified) ───${NC}"
if [ -f /tmp/stress-hey.txt ]; then
    grep -E "FINAL RESULTS|Sent:|HTTP 200:|body_valid:|body_empty:|body_invalid:|body_rpc_error:|HTTP 5xx:|Real success|Fake 200" /tmp/stress-hey.txt | head -20
fi

# WebSocket
echo ""
echo -e "${BOLD}─── WebSocket Stress ───${NC}"
if [ -f /tmp/stress-ws.txt ]; then
    grep -E "Final Summary|Total Sent|Total Received|Total Errors|Connect Errors|Disconnections" /tmp/stress-ws.txt
fi

# Claim/proof from Loki (survives chaos pod kills)
echo ""
echo -e "${BOLD}─── Claim/Proof Pipeline (from Loki) ───${NC}"
END_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')

FINAL_PANICS=$(loki_count "relayer" '|= "PANIC RECOVERED"')
FINAL_WS_RACE=$(loki_count "relayer" '|= "concurrent write"')
FINAL_FLUSHES=$(loki_count "miner" '|= "flushed SMST"')
FINAL_CLAIMS=$(loki_count "miner" '|= "batched claims submitted successfully"')
FINAL_CLAIM_BATCH=$(loki_count "miner" '|= "claim batch complete"')
FINAL_PROOF_EXEC=$(loki_count "miner" '|= "executing batched proof transition"')
FINAL_PROOF_CLOSED=$(loki_count "miner" '|= "proof_window_closed"')
FINAL_SKIPS=$(loki_count "miner" '|= "SKIP UNPROFITABLE"')
FINAL_EMPTY=$(loki_count "miner" '|= "SMST tree is empty"')
FINAL_CLAIM_ERRS=$(loki_count "miner" '|= "claim_not_found"')
OLD_FLOW=$(loki_count "miner" '|= "skipping claim - session has 0 relays"')
TRANSITIONS=$(loki_count "miner" '|= "session transition determined"')
LIFECYCLE_CHECKS=$(loki_count "miner" '|= "lifecycle_check"')

echo "  Block range:          $START_HEIGHT → $END_HEIGHT ($((END_HEIGHT - START_HEIGHT)) blocks)"
echo "  Sessions covered:     ~$(( (END_HEIGHT - START_HEIGHT) / 10 )) (10 blocks/session)"
echo ""
echo "  SMST flushes:         $FINAL_FLUSHES"
echo "  Claims submitted:     $FINAL_CLAIMS"
echo "  Claim batches:        $FINAL_CLAIM_BATCH"
echo "  Proof executions:     $FINAL_PROOF_EXEC"
echo "  Proof window closed:  $FINAL_PROOF_CLOSED"
echo "  Unprofitable skips:   $FINAL_SKIPS"
echo "  Empty tree skips:     $FINAL_EMPTY"
echo "  claim_not_found:      $FINAL_CLAIM_ERRS"
echo "  Transitions logged:   $TRANSITIONS"
echo "  Lifecycle checks:     $LIFECYCLE_CHECKS"
echo ""
echo "  Relayer panics:       $FINAL_PANICS"
echo "  WS concurrent write:  $FINAL_WS_RACE"
echo "  Old snapshot flow:    $OLD_FLOW (should be 0)"

# On-chain verification
echo ""
echo -e "${BOLD}─── On-Chain Verification ───${NC}"
ONCHAIN_CLAIMS=0; ONCHAIN_PROOFS=0; ONCHAIN_SETTLED=0; ONCHAIN_EXPIRED=0
for h in $(seq "$START_HEIGHT" "$END_HEIGHT"); do
    R=$(curl -s "http://localhost:26657/block_results?height=$h" 2>/dev/null)
    C=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventClaimCreated")] | length' 2>/dev/null || echo 0)
    P=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventProofSubmitted")] | length' 2>/dev/null || echo 0)
    S=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimSettled")] | length' 2>/dev/null || echo 0)
    E=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimExpired")] | length' 2>/dev/null || echo 0)
    ONCHAIN_CLAIMS=$((ONCHAIN_CLAIMS + C)); ONCHAIN_PROOFS=$((ONCHAIN_PROOFS + P))
    ONCHAIN_SETTLED=$((ONCHAIN_SETTLED + S)); ONCHAIN_EXPIRED=$((ONCHAIN_EXPIRED + E))
    [ "$C" -gt 0 ] || [ "$P" -gt 0 ] || [ "$S" -gt 0 ] || [ "$E" -gt 0 ] && \
        echo "  h=$h: claims=$C proofs=$P settled=$S expired=$E"
done
echo ""
echo "  Total claims on-chain:   $ONCHAIN_CLAIMS"
echo "  Total proofs on-chain:   $ONCHAIN_PROOFS"
echo "  Total settled:           $ONCHAIN_SETTLED"
echo "  Total expired:           $ONCHAIN_EXPIRED"
echo "  Proof success rate:      $(( ONCHAIN_PROOFS * 100 / (ONCHAIN_CLAIMS > 0 ? ONCHAIN_CLAIMS : 1) ))%"

# ─── Verdict ─────────────────────────────────────────────────
log_phase "VERDICT"

PASS=true

if [ "$FINAL_PANICS" -gt 0 ]; then
    log_error "FAIL: Relayer panics detected ($FINAL_PANICS)"
    PASS=false
fi

if [ "$FINAL_WS_RACE" -gt 0 ]; then
    log_error "FAIL: WebSocket concurrent write errors"
    PASS=false
fi

if [ "$FINAL_FLUSHES" -eq 0 ]; then
    log_error "FAIL: No SMST flushes — claim pipeline not running"
    PASS=false
fi

if [ "$ONCHAIN_CLAIMS" -eq 0 ]; then
    log_error "FAIL: No claims on-chain"
    PASS=false
fi

if [ "$ONCHAIN_EXPIRED" -gt 0 ]; then
    log_error "FAIL: $ONCHAIN_EXPIRED claims expired (PROOF_MISSING penalty)"
    PASS=false
fi

if [ "$OLD_FLOW" -gt 0 ]; then
    log_error "FAIL: Old snapshot-based skip flow detected"
    PASS=false
fi

if [ "$ONCHAIN_CLAIMS" -gt 0 ] && [ "$ONCHAIN_PROOFS" -eq "$ONCHAIN_CLAIMS" ]; then
    log_info "PERFECT: All $ONCHAIN_CLAIMS claims have matching proofs"
fi

if [ "$ONCHAIN_SETTLED" -gt 0 ]; then
    log_info "Settled: $ONCHAIN_SETTLED claims paid out"
fi

if [ "$ONCHAIN_PROOFS" -gt 0 ]; then
    log_info "Proofs submitted: $ONCHAIN_PROOFS — proof pipeline working"
fi

echo ""
if $PASS; then
    echo -e "${GREEN}${BOLD}  ✓ ALL CHECKS PASSED${NC}"
    exit 0
else
    echo -e "${RED}${BOLD}  ✗ SOME CHECKS FAILED${NC}"
    exit 1
fi
