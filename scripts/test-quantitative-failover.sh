#!/usr/bin/env bash
#
# Quantitative HA failover test.
#
# Question: after a mid-flight miner scale-down (2 replicas -> 1), does the
# total number of on-chain claimed relays equal the number of successful
# relays reported by the HTTP loader?
#
# The broader chaos tests answer "did the proof land?". This one answers
# "did we count every relay the loader thinks succeeded?". Any material
# drift flags either:
#   - relay loss during failover (survivor skipped stream entries), or
#   - WAL redelivery double-counting (dedup missed), or
#   - late-arriving relays rejected by the new leader (expected, bounded).
#
# Success criteria:
#   |ON_CHAIN_RELAYS - body_valid| / body_valid <= MAX_DRIFT_PCT/100
#
# Drift is bidirectional: either direction is noise at low %, a bug at high %.
#   - Under-count signals relay loss during failover (survivor skipped stream
#     entries) or WAL redelivery that dedup erroneously dropped.
#   - Over-count signals relays the relayer processed successfully but whose
#     HTTP response never reached the client (timeouts, connection drops -
#     NOT a supplier-side bug) OR genuine WAL double-counting from a dedup
#     miss (which IS a bug). At low %, it is overwhelmingly the former;
#     investigate if drift spikes.
#
# MAX_DRIFT_PCT defaults to 5%. Set MAX_DRIFT_PCT=0 for a strict run.
#
# Usage:
#   ./scripts/test-quantitative-failover.sh
#   DURATION=120 HTTP_RPS=300 MAX_LOSS_PCT=2 ./scripts/test-quantitative-failover.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# --- configuration ---
GATEWAY_URL="${GATEWAY_URL:-http://localhost:3069/v1}"
HTTP_SERVICE="${HTTP_SERVICE:-develop-http}"
K8S_CONTEXT="${K8S_CONTEXT:-kind-kind}"

HTTP_RPS="${HTTP_RPS:-200}"
HTTP_WORKERS="${HTTP_WORKERS:-80}"
DURATION="${DURATION:-90}"           # total load window
KILL_AT="${KILL_AT:-30}"              # scale-down at T=KILL_AT seconds
POST_WAIT="${POST_WAIT:-150}"         # wait for last session's full lifecycle
MAX_DRIFT_PCT="${MAX_DRIFT_PCT:-5}"

# KILL_TARGET controls which miner is terminated at T=KILL_AT:
#   any    (default) - kubectl scale down to 1, k8s picks the pod.
#                     Covers follower-dies + rebalance scenario (if k8s
#                     picks the follower) or leader-dies + election
#                     (if k8s picks the leader).
#   leader         - explicitly delete the leader pod first (guarantees
#                     election + lazy-load), then scale to 1. Use to
#                     force the path the chaos-leader-flush-gap test
#                     covers qualitatively.
KILL_TARGET="${KILL_TARGET:-any}"
# Kept for backward compatibility; MAX_DRIFT_PCT takes precedence if set.
MAX_LOSS_PCT="${MAX_LOSS_PCT:-$MAX_DRIFT_PCT}"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; MAGENTA='\033[0;35m'; BOLD='\033[1m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date +%H:%M:%S) $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date +%H:%M:%S) $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date +%H:%M:%S) $1"; }
log_scale() { echo -e "${MAGENTA}[SCALE]${NC} $(date +%H:%M:%S) $1"; }
log_phase() { echo -e "\n${BLUE}${BOLD}=== $1 ===${NC}\n"; }

# Helpers
leader_pod() {
    local val
    val=$(redis-cli GET ha:miner:global_leader 2>/dev/null | tr -d '"')
    [ -z "$val" ] && { echo ""; return; }
    local podname="${val%-*}"
    kubectl --context "$K8S_CONTEXT" get pod "$podname" --no-headers 2>/dev/null | awk 'NR==1 {print $1}'
}

LOADER_PID=""
ORIG_REPLICAS=""
cleanup() {
    [ -n "$LOADER_PID" ] && kill "$LOADER_PID" 2>/dev/null || true
    # Best-effort: restore original replica count if we scaled down
    if [ -n "$ORIG_REPLICAS" ]; then
        kubectl --context "$K8S_CONTEXT" scale deploy/miner --replicas="$ORIG_REPLICAS" 2>&1 | head -1 || true
    fi
    wait 2>/dev/null || true
}
trap cleanup EXIT

# --- pre-flight ---
log_phase "PRE-FLIGHT"

command -v jq >/dev/null || { log_error "jq required"; exit 2; }
command -v redis-cli >/dev/null || { log_error "redis-cli required"; exit 2; }

# Relay smoke test with body verification
for i in $(seq 1 15); do
    BODY=$(curl -s -m 5 -X POST \
        -H "Content-Type: application/json" -H "Target-Service-Id: $HTTP_SERVICE" \
        -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
        "$GATEWAY_URL" 2>/dev/null || true)
    if echo "$BODY" | grep -q '"result"'; then
        log_info "Relay OK (attempt $i)"
        break
    fi
    log_warn "Relay not ready (attempt $i/15): [${BODY:0:60}]"
    sleep 2
done
echo "$BODY" | grep -q '"result"' || { log_error "Relay never returned valid body"; exit 1; }

ORIG_REPLICAS=$(kubectl --context "$K8S_CONTEXT" get deploy/miner -o jsonpath='{.spec.replicas}' 2>/dev/null)
[ "$ORIG_REPLICAS" != "2" ] && { log_warn "Miner replicas=$ORIG_REPLICAS (expected 2); test still runs but 'scale down' is less meaningful"; }
log_info "Miner replicas currently: $ORIG_REPLICAS"

# Wait for leader election
INITIAL_LEADER=""
for i in $(seq 1 20); do
    INITIAL_LEADER=$(leader_pod)
    [ -n "$INITIAL_LEADER" ] && break
    sleep 3
done
[ -z "$INITIAL_LEADER" ] && { log_error "No leader found in Redis"; exit 1; }
log_info "Initial leader: $INITIAL_LEADER"

START_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "0"')
log_info "Start block height: $START_HEIGHT"

TOTAL_EXPECTED=$((HTTP_RPS * DURATION))
echo ""
echo -e "${BOLD}Test parameters:${NC}"
echo "  HTTP_RPS:           $HTTP_RPS"
echo "  Duration:           ${DURATION}s (expected ~${TOTAL_EXPECTED} relays sent)"
echo "  Scale-down at:      T=${KILL_AT}s (deploy/miner --replicas=1)"
echo "  Post-load wait:     ${POST_WAIT}s"
echo "  Max drift tolerance: ${MAX_DRIFT_PCT}% (bidirectional)"
echo ""

# --- phase 1: launch load ---
log_phase "LAUNCHING LOAD"

LOADER_OUT=/tmp/quant-loader.txt
rm -f "$LOADER_OUT"
go run "$SCRIPT_DIR/loadtest/http-verify.go" \
    -url "$GATEWAY_URL" \
    -service "$HTTP_SERVICE" \
    -rps "$HTTP_RPS" \
    -workers "$HTTP_WORKERS" \
    -duration "${DURATION}s" \
    -report 15 > "$LOADER_OUT" 2>&1 &
LOADER_PID=$!
log_info "Loader PID: $LOADER_PID, writing to $LOADER_OUT"

# --- phase 2: mid-flight scale-down ---
log_phase "MID-FLIGHT SCALE-DOWN (T=${KILL_AT}s)"
sleep "$KILL_AT"

PRE_SCALE_LEADER=$(leader_pod)
log_scale "Pre-scale leader: $PRE_SCALE_LEADER"
KILL_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')

if [ "$KILL_TARGET" = "leader" ]; then
    log_scale "KILL_TARGET=leader: deleting leader pod $PRE_SCALE_LEADER at height $KILL_HEIGHT"
    kubectl --context "$K8S_CONTEXT" delete pod "$PRE_SCALE_LEADER" --grace-period=5 --wait=false 2>&1 | head -1
    # Wait a few seconds for k8s to create a replacement (since replicas=2),
    # then scale to 1 so k8s picks the NEWER replacement (default behavior)
    # and leaves the original follower (now elected leader) alive.
    sleep 5
    log_scale "Scaling deploy/miner to 1 replica (k8s will terminate the replacement pod, not the former follower)"
    kubectl --context "$K8S_CONTEXT" scale deploy/miner --replicas=1 2>&1 | head -1
else
    log_scale "KILL_TARGET=any: scaling deploy/miner to 1 replica at height $KILL_HEIGHT (k8s picks)"
    kubectl --context "$K8S_CONTEXT" scale deploy/miner --replicas=1 2>&1 | head -1
fi

# Wait until only 1 miner is Running
for i in $(seq 1 30); do
    RUNNING=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | grep -c Running)
    if [ "$RUNNING" = "1" ]; then
        SURVIVOR=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | awk '/Running/ {print $1}' | head -1)
        log_scale "Scale-down complete after ${i}s - survivor: $SURVIVOR"
        if [ "$SURVIVOR" = "$PRE_SCALE_LEADER" ]; then
            log_scale "Survivor WAS the leader - no election, supplier rebalance only"
        else
            log_scale "Survivor was the FOLLOWER - leader election will promote it"
        fi
        break
    fi
    sleep 1
done

# --- phase 3: continue load until duration elapsed ---
log_phase "CONTINUING LOAD THROUGH FAILOVER"

# Wait for loader to finish (AfterFunc in loader stops at DURATION from its start)
wait "$LOADER_PID" 2>/dev/null || true
LOADER_PID=""
log_info "Load window complete"

END_OF_LOAD_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')

# --- phase 4: wait for on-chain settlement ---
log_phase "WAITING ${POST_WAIT}s FOR FULL LIFECYCLE (claim+proof+settle)"
sleep "$POST_WAIT"
END_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')
log_info "End block height: $END_HEIGHT"

# --- phase 5: extract loader stats ---
log_phase "LOADER RESULT"

SENT=$(grep -E '^\s+Sent:' "$LOADER_OUT" | awk '{print $2}')
BODY_VALID=$(grep -E 'body_valid:' "$LOADER_OUT" | awk '{print $2}')
BODY_EMPTY=$(grep -E 'body_empty:' "$LOADER_OUT" | awk '{print $2}')
BODY_INVALID=$(grep -E 'body_invalid:' "$LOADER_OUT" | awk '{print $2}')
BODY_RPC_ERR=$(grep -E 'body_rpc_error:' "$LOADER_OUT" | awk '{print $2}')
CONN_ERRS=$(grep -E '^\s+Connection errs:' "$LOADER_OUT" | awk '{print $3}')

echo "  sent:           ${SENT:-?}"
echo "  body_valid:     ${BODY_VALID:-?}"
echo "  body_empty:     ${BODY_EMPTY:-?}"
echo "  body_invalid:   ${BODY_INVALID:-?}"
echo "  body_rpc_error: ${BODY_RPC_ERR:-?}"
echo "  conn_errors:    ${CONN_ERRS:-?}"

[ -z "$BODY_VALID" ] && { log_error "loader did not report body_valid - cannot continue"; cat "$LOADER_OUT" | tail -20; exit 1; }
[ "$BODY_VALID" = "0" ] && { log_error "body_valid=0 - loader is broken or PATH is not routing; see $LOADER_OUT"; exit 1; }

# --- phase 6: on-chain sum ---
log_phase "ON-CHAIN AGGREGATE (heights $START_HEIGHT..$END_HEIGHT)"

ON_CHAIN_RELAYS=0
CLAIM_COUNT=0
PROOF_COUNT=0
SETTLED_COUNT=0
EXPIRED_COUNT=0

for h in $(seq "$START_HEIGHT" "$END_HEIGHT"); do
    R=$(curl -s "http://localhost:26657/block_results?height=$h" 2>/dev/null)
    [ -z "$R" ] && continue

    # Sum num_relays across all ClaimCreated events in this block.
    # num_relays comes as a JSON-stringified integer like "\"42\"" so we
    # strip the wrapping quotes before summing.
    BLOCK_RELAYS=$(echo "$R" | jq '
        [.result.txs_results[]?
            | select(.code == 0)
            | .events[]?
            | select(.type == "pocket.proof.EventClaimCreated")
            | .attributes[]?
            | select(.key == "num_relays")
            | .value | fromjson | tonumber
        ] | add // 0' 2>/dev/null || echo 0)
    BLOCK_CLAIMS=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventClaimCreated")] | length' 2>/dev/null || echo 0)
    BLOCK_PROOFS=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventProofSubmitted")] | length' 2>/dev/null || echo 0)
    BLOCK_SETTLED=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimSettled")] | length' 2>/dev/null || echo 0)
    BLOCK_EXPIRED=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimExpired")] | length' 2>/dev/null || echo 0)

    ON_CHAIN_RELAYS=$((ON_CHAIN_RELAYS + BLOCK_RELAYS))
    CLAIM_COUNT=$((CLAIM_COUNT + BLOCK_CLAIMS))
    PROOF_COUNT=$((PROOF_COUNT + BLOCK_PROOFS))
    SETTLED_COUNT=$((SETTLED_COUNT + BLOCK_SETTLED))
    EXPIRED_COUNT=$((EXPIRED_COUNT + BLOCK_EXPIRED))

    if [ "$BLOCK_CLAIMS" -gt 0 ] || [ "$BLOCK_PROOFS" -gt 0 ] || [ "$BLOCK_SETTLED" -gt 0 ] || [ "$BLOCK_EXPIRED" -gt 0 ]; then
        echo "  h=$h: relays=$BLOCK_RELAYS claims=$BLOCK_CLAIMS proofs=$BLOCK_PROOFS settled=$BLOCK_SETTLED expired=$BLOCK_EXPIRED"
    fi
done

echo ""
echo "  Total claims on-chain:    $CLAIM_COUNT"
echo "  Total proofs on-chain:    $PROOF_COUNT"
echo "  Total settled:            $SETTLED_COUNT"
echo "  Total expired:            $EXPIRED_COUNT"
echo -e "  ${BOLD}Total claimed relays:     $ON_CHAIN_RELAYS${NC}"

# --- phase 7: verdict ---
log_phase "VERDICT"

PASS=true

if [ "$EXPIRED_COUNT" -gt 0 ]; then
    log_error "FAIL: $EXPIRED_COUNT claims expired (PROOF_MISSING)"
    PASS=false
fi

# Bidirectional drift tolerance: both under- and over-count get the same
# cap. Under-count in a small amount is usually session-boundary; over-count
# in a small amount is usually client-side timeouts on a relay the relayer
# fully processed and signed. Either direction at >MAX_DRIFT_PCT signals a
# real bug.
DRIFT_SIGNED=$((ON_CHAIN_RELAYS - BODY_VALID))
DRIFT=$DRIFT_SIGNED
if [ "$DRIFT" -lt 0 ]; then DRIFT=$((-DRIFT)); fi
DRIFT_PCT_X100=$((DRIFT * 10000 / BODY_VALID))  # basis points
DRIFT_PCT=$((DRIFT_PCT_X100 / 100))
DRIFT_PCT_REM=$((DRIFT_PCT_X100 % 100))

echo "  body_valid (client-observed): $BODY_VALID"
echo "  on_chain (supplier-observed): $ON_CHAIN_RELAYS"
if [ "$DRIFT_SIGNED" -ge 0 ]; then
    echo "  drift:                        +$DRIFT relays (${DRIFT_PCT}.${DRIFT_PCT_REM}%) - supplier counted more than client saw"
    echo "                                (usually HTTP response lost on client side while relayer fully signed)"
else
    echo "  drift:                        -$DRIFT relays (${DRIFT_PCT}.${DRIFT_PCT_REM}%) - client saw more than supplier counted"
    echo "                                (late-session rejects or failover lost stream entries - investigate at scale)"
fi

MAX_DRIFT_BASIS_POINTS=$((MAX_DRIFT_PCT * 100))
if [ "$DRIFT_PCT_X100" -gt "$MAX_DRIFT_BASIS_POINTS" ]; then
    log_error "FAIL: drift ${DRIFT_PCT}.${DRIFT_PCT_REM}% exceeds max ${MAX_DRIFT_PCT}% - relay counts diverged too far across failover"
    PASS=false
fi

echo ""
if $PASS; then
    echo -e "${GREEN}${BOLD}  PASS: HA failover preserved relay counts end-to-end${NC}"
    echo "    body_valid=$BODY_VALID, on_chain_relays=$ON_CHAIN_RELAYS (drift ${DRIFT_PCT}.${DRIFT_PCT_REM}%)"
    echo "    claims=$CLAIM_COUNT, proofs=$PROOF_COUNT, settled=$SETTLED_COUNT, expired=$EXPIRED_COUNT"
    exit 0
else
    echo -e "${RED}${BOLD}  FAIL: quantitative HA failover test failed${NC}"
    exit 1
fi
