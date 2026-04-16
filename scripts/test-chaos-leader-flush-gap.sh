#!/usr/bin/env bash
#
# Targeted chaos: validates SMST lazy-load-from-Redis HA failover path
# (bug #4 fix in miner/smst_manager.go).
#
# Scenario: kill the miner leader in the window AFTER "flushed SMST"
# (claim submitted) but BEFORE "executing batched proof transition".
# The follower is promoted and must lazy-load the tree from Redis to
# generate the proof. If the lazy-load path has a regression, proofs
# never reach chain and claims expire with PROOF_MISSING.
#
# Why this test matters: the broader stress+chaos suite kills miners
# but can't guarantee the timing hits this specific gap — in the
# 15:56 run, lazy_loaded count was 0 because every kill landed outside
# the flush→proof gap (or hit a non-leader pod).
#
# Timing (with current localnet genesis):
#   num_blocks_per_session:          10
#   claim_window_open_offset_blocks: 1   → flush at block 10N+1
#   claim_window_close_offset:       8
#   proof_window_open_offset:        0   → proof window opens at 10N+9
#   proof_window_close_offset:       8   → proof window closes at 10N+17
#   leader_ttl_seconds:              30  (config)
#   heartbeat_rate_seconds:          10  (config)
#
# The proof window (~8s) is narrower than leader TTL (30s), so SIGKILL
# would leave the lock orphaned past the proof window. We use graceful
# SIGTERM with --grace-period=5 so the leader runs its Close() path and
# releases the lock immediately. The follower picks up on its next
# heartbeat tick (≤10s).
#
# Success criteria:
#   1. lazy-loaded SMST tree from Redis (for proof generation) > 0
#   2. ClaimCreated on-chain == ProofSubmitted on-chain
#   3. ClaimExpired on-chain == 0
#   4. Zero "batched proof callback failed" / "root mismatch" / "session not found"
#
# Usage:
#   ./scripts/test-chaos-leader-flush-gap.sh
#   DURATION=240 MAX_KILLS=3 ./scripts/test-chaos-leader-flush-gap.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# ─── Configuration ───────────────────────────────────────────
GATEWAY_URL="${GATEWAY_URL:-http://localhost:3069/v1}"
HTTP_SERVICE="${HTTP_SERVICE:-develop-http}"
K8S_CONTEXT="${K8S_CONTEXT:-kind-kind}"
LOKI_URL="${LOKI_URL:-http://localhost:3100}"
REDIS_CMD="${REDIS_CMD:-redis-cli}"

# Load params
HTTP_RPS="${HTTP_RPS:-300}"
HTTP_WORKERS="${HTTP_WORKERS:-100}"
DURATION="${DURATION:-180}"            # covers ~2 session cycles (10 blocks each)
MAX_KILLS="${MAX_KILLS:-3}"             # kill leader after N flushes observed
POLL_INTERVAL="${POLL_INTERVAL:-2}"     # seconds between flush-checks
KILL_GRACE="${KILL_GRACE:-5}"           # k8s grace period (SIGTERM→SIGKILL)
POST_WAIT="${POST_WAIT:-90}"            # after load ends, wait for on-chain settlement
                                        # must exceed claim_window_close + proof_window_close
                                        # (8 + 8 = 16 blocks ~ 16s) plus buffer for the last
                                        # claim batch submitted at the end of load

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; MAGENTA='\033[0;35m'; BOLD='\033[1m'; NC='\033[0m'
log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date +%H:%M:%S) $1"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC}  $(date +%H:%M:%S) $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $(date +%H:%M:%S) $1"; }
log_kill()  { echo -e "${MAGENTA}[KILL]${NC}  $(date +%H:%M:%S) $1"; }
log_phase() { echo -e "\n${BLUE}${BOLD}═══ $1 ═══${NC}\n"; }

# ─── Helpers ─────────────────────────────────────────────────

# Loki count since LOKI_START_NS (set in pre-flight)
loki_count() {
    local app="$1" filter="$2"
    local now_ns
    now_ns=$(date +%s)000000000
    curl -sG "${LOKI_URL}/loki/api/v1/query_range" \
        --data-urlencode "query={app=\"${app}\"} ${filter}" \
        --data-urlencode "limit=5000" \
        --data-urlencode "start=${LOKI_START_NS}" \
        --data-urlencode "end=${now_ns}" 2>/dev/null | \
        jq -r '[.data.result[].values | length] | add // 0' 2>/dev/null || echo "0"
}

loki_lines() {
    local app="$1" filter="$2" limit="${3:-10}"
    local now_ns
    now_ns=$(date +%s)000000000
    curl -sG "${LOKI_URL}/loki/api/v1/query_range" \
        --data-urlencode "query={app=\"${app}\"} ${filter}" \
        --data-urlencode "limit=${limit}" \
        --data-urlencode "start=${LOKI_START_NS}" \
        --data-urlencode "end=${now_ns}" 2>/dev/null | \
        jq -r '.data.result[].values[][1]' 2>/dev/null
}

# Resolve current leader instance_id → k8s pod name.
# global_leader value format: "<hostname>-<pid>" where hostname == pod name.
leader_pod() {
    local val podname
    val=$("$REDIS_CMD" GET ha:miner:global_leader 2>/dev/null | tr -d '"')
    [ -z "$val" ] && { echo ""; return; }
    # Strip trailing "-<pid>" (last dash-separated segment).
    podname="${val%-*}"
    kubectl --context "$K8S_CONTEXT" get pod "$podname" --no-headers 2>/dev/null | awk 'NR==1 {print $1}'
}

LOAD_PID=""
cleanup() {
    [ -n "$LOAD_PID" ] && kill "$LOAD_PID" 2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

# ─── Pre-flight ──────────────────────────────────────────────
log_phase "PRE-FLIGHT"

command -v jq >/dev/null || { log_error "jq required"; exit 2; }
command -v "$REDIS_CMD" >/dev/null || { log_error "redis-cli required"; exit 2; }

# Pre-flight: wait up to 30s for a valid body (PATH may still be syncing sessions).
RELAY_OK=false
for i in $(seq 1 15); do
    BODY=$(curl -s -m 5 -X POST \
        -H "Content-Type: application/json" -H "Target-Service-Id: $HTTP_SERVICE" \
        -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
        "$GATEWAY_URL" 2>/dev/null || true)
    if echo "$BODY" | grep -q '"result"'; then
        RELAY_OK=true
        log_info "Relay OK (attempt $i)"
        break
    fi
    log_warn "Relay not ready yet (attempt $i/15, body=[${BODY:0:80}])"
    sleep 2
done
$RELAY_OK || { log_error "Relay never returned a valid body in 30s"; exit 1; }

MINER_PODS=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | grep -c Running || true)
[ "$MINER_PODS" -lt 2 ] && {
    log_error "Need ≥2 miner pods for HA failover test (got $MINER_PODS)"
    exit 1
}
log_info "Miner pods Running: $MINER_PODS"

INITIAL_LEADER=""
for i in $(seq 1 20); do
    INITIAL_LEADER=$(leader_pod)
    [ -n "$INITIAL_LEADER" ] && break
    log_warn "Waiting for leader election (attempt $i/20)..."
    sleep 3
done
[ -z "$INITIAL_LEADER" ] && { log_error "No leader in Redis after 60s — cannot target"; exit 1; }
log_info "Initial leader: $INITIAL_LEADER"

TTL_MS=$("$REDIS_CMD" PTTL ha:miner:global_leader 2>/dev/null || echo 0)
log_info "Leader lock TTL remaining: ${TTL_MS}ms"

START_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "0"')
log_info "Start height: $START_HEIGHT"

LOKI_START_NS=$(date +%s)000000000

echo ""
echo -e "${BOLD}  Test parameters:${NC}"
echo "    HTTP RPS:         $HTTP_RPS"
echo "    Duration:         ${DURATION}s"
echo "    Max kills:        $MAX_KILLS"
echo "    Kill grace:       ${KILL_GRACE}s (SIGTERM for lock release)"
echo "    Post-load wait:   ${POST_WAIT}s (on-chain settlement)"
echo ""

# ─── Phase 1: Launch load ────────────────────────────────────
log_phase "LAUNCHING LOAD (${DURATION}s)"

log_info "HTTP: $HTTP_RPS RPS × $HTTP_WORKERS workers"
go run "$SCRIPT_DIR/loadtest/http-verify.go" \
    -url "$GATEWAY_URL" \
    -service "$HTTP_SERVICE" \
    -rps "$HTTP_RPS" \
    -workers "$HTTP_WORKERS" \
    -duration "${DURATION}s" \
    -report 30 > /tmp/leader-chaos-load.txt 2>&1 &
LOAD_PID=$!
log_info "Load PID: $LOAD_PID"

# ─── Phase 2: Watch for flushes, kill leader on each ────────
log_phase "HUNTING FLUSHES (kill leader in flush→proof gap)"

KILLS=0
LAST_FLUSH_COUNT=0
DEADLINE=$(($(date +%s) + DURATION))
KILL_LOG=()

while [ "$(date +%s)" -lt "$DEADLINE" ]; do
    sleep "$POLL_INTERVAL"

    FLUSH_NOW=$(loki_count "miner" '|= "flushed SMST - root hash ready for claim"')
    if [ "$FLUSH_NOW" -gt "$LAST_FLUSH_COUNT" ]; then
        NEW=$((FLUSH_NOW - LAST_FLUSH_COUNT))
        LAST_FLUSH_COUNT="$FLUSH_NOW"

        if [ "$KILLS" -ge "$MAX_KILLS" ]; then
            log_info "Flush #$FLUSH_NOW (+$NEW) — MAX_KILLS reached, observing only"
            continue
        fi

        LEADER_NOW=$(leader_pod)
        if [ -z "$LEADER_NOW" ]; then
            log_warn "Flush (+$NEW) but no leader resolvable — skipping this round"
            continue
        fi

        KILLS=$((KILLS + 1))
        HEIGHT_AT_KILL=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')
        log_kill "[${KILLS}/${MAX_KILLS}] flush total=$FLUSH_NOW (+$NEW), killing leader $LEADER_NOW at height $HEIGHT_AT_KILL"
        kubectl --context "$K8S_CONTEXT" delete pod "$LEADER_NOW" --grace-period="$KILL_GRACE" --wait=false 2>&1 | head -2

        KILL_LOG+=("kill#${KILLS}: pod=$LEADER_NOW height=$HEIGHT_AT_KILL flushes_at_kill=$FLUSH_NOW")

        # Give the system a moment to observe the failover before the next poll
        sleep 3
    fi
done

log_info "Load window complete. Kills issued: $KILLS"

# Wait for load to finish
wait "$LOAD_PID" 2>/dev/null || true
LOAD_PID=""

# Give chain time to settle final proofs
log_info "Waiting ${POST_WAIT}s for on-chain settlement events..."
sleep "$POST_WAIT"

END_HEIGHT=$(curl -s http://localhost:26657/status 2>/dev/null | jq -r '.result.sync_info.latest_block_height // "?"')

# ─── Phase 3: Results ────────────────────────────────────────
log_phase "RESULTS"

# Log signals — these are the bug-fix markers
LAZY_PROOF=$(loki_count "miner" '|= "lazy-loaded SMST tree from Redis for proof generation"')
LAZY_ROOT=$(loki_count "miner" '|= "lazy-loaded SMST tree from Redis for GetTreeRoot"')
LAZY_FAIL=$(loki_count "miner" '|~ "session.*not found in memory or Redis"')

FLUSHES=$(loki_count "miner" '|= "flushed SMST - root hash ready for claim"')
CLAIM_OK=$(loki_count "miner" '|= "batched claims submitted successfully"')
CLAIM_BATCH=$(loki_count "miner" '|= "claim batch complete"')
PROOF_EXEC=$(loki_count "miner" '|= "executing batched proof transition"')
PROOF_CALLBACK_FAIL=$(loki_count "miner" '|= "batched proof callback failed"')
ROOT_MISMATCH=$(loki_count "miner" '|= "root mismatch"')

LEADER_ACQ=$(loki_count "miner" '|= "acquired global leadership"')
LEADER_LOST=$(loki_count "miner" '|= "lost global leadership"')

PANICS=$(loki_count "miner" '|= "panic"')

echo -e "${BOLD}─── Kill timeline ───${NC}"
if [ "${#KILL_LOG[@]}" -eq 0 ]; then
    echo "  (no kills issued — flushes never observed)"
else
    for line in "${KILL_LOG[@]}"; do echo "  $line"; done
fi

echo ""
echo -e "${BOLD}─── HA Failover Signals (from Loki) ───${NC}"
echo "  Flushes (claim):                         $FLUSHES"
echo "  Claims submitted (miner):                $CLAIM_OK"
echo "  Claim batches complete:                  $CLAIM_BATCH"
echo "  Proof executions (miner):                $PROOF_EXEC"
echo "  Leader acquisitions:                     $LEADER_ACQ"
echo "  Leadership losses:                       $LEADER_LOST"
echo ""
echo -e "  ${BOLD}lazy-loaded for proof generation:        $LAZY_PROOF   ← key signal${NC}"
echo "  lazy-loaded for GetTreeRoot:             $LAZY_ROOT"
echo "  session not found in memory or Redis:    $LAZY_FAIL   (must be 0)"
echo "  batched proof callback failed:           $PROOF_CALLBACK_FAIL   (must be 0)"
echo "  root mismatch:                           $ROOT_MISMATCH   (must be 0)"
echo "  miner panics:                            $PANICS   (must be 0)"

# On-chain verification
echo ""
echo -e "${BOLD}─── On-Chain Verification ───${NC}"
ONCHAIN_CLAIMS=0; ONCHAIN_PROOFS=0; ONCHAIN_SETTLED=0; ONCHAIN_EXPIRED=0
EXPIRED_REASONS=()
for h in $(seq "$START_HEIGHT" "$END_HEIGHT"); do
    R=$(curl -s "http://localhost:26657/block_results?height=$h" 2>/dev/null)
    [ -z "$R" ] && continue
    C=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventClaimCreated")] | length' 2>/dev/null || echo 0)
    P=$(echo "$R" | jq '[.result.txs_results[]? | select(.code == 0) | .events[]? | select(.type == "pocket.proof.EventProofSubmitted")] | length' 2>/dev/null || echo 0)
    S=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimSettled")] | length' 2>/dev/null || echo 0)
    E=$(echo "$R" | jq '[.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimExpired")] | length' 2>/dev/null || echo 0)
    ONCHAIN_CLAIMS=$((ONCHAIN_CLAIMS + C)); ONCHAIN_PROOFS=$((ONCHAIN_PROOFS + P))
    ONCHAIN_SETTLED=$((ONCHAIN_SETTLED + S)); ONCHAIN_EXPIRED=$((ONCHAIN_EXPIRED + E))
    if [ "$E" -gt 0 ]; then
        REASON=$(echo "$R" | jq -r '.result.finalize_block_events[]? | select(.type == "pocket.tokenomics.EventClaimExpired") | .attributes[]? | select(.key == "expiration_reason") | .value' 2>/dev/null | head -1)
        EXPIRED_REASONS+=("h=$h: $REASON")
    fi
    if [ "$C" != "0" ] || [ "$P" != "0" ] || [ "$S" != "0" ] || [ "$E" != "0" ]; then
        echo "  h=$h: claims=$C proofs=$P settled=$S expired=$E"
    fi
done

echo ""
echo "  Block range:        $START_HEIGHT → $END_HEIGHT"
echo "  Claims on-chain:    $ONCHAIN_CLAIMS"
echo "  Proofs on-chain:    $ONCHAIN_PROOFS"
echo "  Settled:            $ONCHAIN_SETTLED"
echo "  Expired:            $ONCHAIN_EXPIRED"
if [ "${#EXPIRED_REASONS[@]}" -gt 0 ]; then
    echo ""
    echo -e "  ${RED}${BOLD}Expiration reasons:${NC}"
    for r in "${EXPIRED_REASONS[@]}"; do echo "    $r"; done
fi

# ─── Verdict ─────────────────────────────────────────────────
log_phase "VERDICT"

PASS=true

if [ "$KILLS" -eq 0 ]; then
    log_error "FAIL: No kills issued — no flushes observed within ${DURATION}s"
    log_error "  HTTP_RPS=$HTTP_RPS may be too low, or load didn't produce sessions"
    PASS=false
fi

if [ "$PANICS" -gt 0 ]; then
    log_error "FAIL: miner panics detected"
    PASS=false
fi

if [ "$PROOF_CALLBACK_FAIL" -gt 0 ]; then
    log_error "FAIL: $PROOF_CALLBACK_FAIL batched proof callback failures"
    loki_lines "miner" '|= "batched proof callback failed"' 5
    PASS=false
fi

if [ "$ROOT_MISMATCH" -gt 0 ]; then
    log_error "FAIL: $ROOT_MISMATCH root mismatch errors (tree differs from claimed root)"
    loki_lines "miner" '|= "root mismatch"' 5
    PASS=false
fi

if [ "$LAZY_FAIL" -gt 0 ]; then
    log_error "FAIL: $LAZY_FAIL 'session not found in memory or Redis' — tree lost entirely"
    loki_lines "miner" '|= "session.*not found in memory or Redis"' 5
    PASS=false
fi

if [ "$ONCHAIN_EXPIRED" -gt 0 ]; then
    log_error "FAIL: $ONCHAIN_EXPIRED claim(s) expired (PROOF_MISSING penalty)"
    log_error "  This is the exact bug the lazy-load fix is supposed to prevent"
    PASS=false
fi

if [ "$ONCHAIN_CLAIMS" -gt 0 ] && [ "$ONCHAIN_PROOFS" -lt "$ONCHAIN_CLAIMS" ]; then
    MISSING=$((ONCHAIN_CLAIMS - ONCHAIN_PROOFS))
    log_error "FAIL: $MISSING claim(s) missing proofs on-chain (claims=$ONCHAIN_CLAIMS, proofs=$ONCHAIN_PROOFS)"
    PASS=false
fi

if [ "$KILLS" -gt 0 ] && [ "$LAZY_PROOF" -eq 0 ]; then
    # Not a failure if proofs all succeeded — means kill didn't hit the right window.
    # But it IS a failure if the point of the test was to exercise the path.
    log_warn "LAZY-LOAD PATH NEVER FIRED despite $KILLS leader kills"
    log_warn "  Likely causes:"
    log_warn "    a) old leader already submitted proof before kill landed"
    log_warn "    b) SIGTERM grace period too long — leader lived through proof window"
    log_warn "    c) kill hit a non-leader pod (leader_pod resolved stale)"
    log_warn "  This is a TEST-COVERAGE failure, not necessarily a code bug."
    log_warn "  Try: increase HTTP_RPS, decrease POLL_INTERVAL, or decrease KILL_GRACE"
    PASS=false
fi

echo ""
if $PASS; then
    echo -e "${GREEN}${BOLD}  ✓ HA FAILOVER (lazy-load) PATH VALIDATED${NC}"
    echo "    kills=$KILLS, lazy_load_events=$LAZY_PROOF"
    echo "    claims=$ONCHAIN_CLAIMS, proofs=$ONCHAIN_PROOFS, settled=$ONCHAIN_SETTLED, expired=0"
    exit 0
else
    echo -e "${RED}${BOLD}  ✗ HA FAILOVER TEST FAILED${NC}"
    exit 1
fi
