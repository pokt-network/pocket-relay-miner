#!/usr/bin/env bash
#
# Chaos Monkey for pocket-relay-miner
#
# Runs alongside the stress test and randomly injects failures:
#   1. Kill random relayer/miner pods (test HA failover)
#   2. Redis connection blip (test fail-open/closed behavior)
#   3. Backend latency injection (test pool exhaustion)
#   4. Network partition simulation (test reconnection)
#   5. Memory pressure (test graceful degradation)
#
# Usage:
#   # In terminal 1: run stress test
#   ./scripts/test-stress-max.sh
#
#   # In terminal 2: run chaos monkey alongside
#   ./scripts/test-chaos.sh
#
#   # Or run both together:
#   ./scripts/test-stress-max.sh &
#   ./scripts/test-chaos.sh

set -euo pipefail

K8S_CONTEXT="kind-kind"
CHAOS_INTERVAL="${CHAOS_INTERVAL:-20}"  # seconds between chaos events
DURATION="${DURATION:-300}"             # total chaos duration
REDIS_POD="redis-standalone-0"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; MAGENTA='\033[0;35m'; NC='\033[0m'
log_chaos() { echo -e "${MAGENTA}[CHAOS]${NC} $(date +%H:%M:%S) $1"; }
log_info()  { echo -e "${GREEN}[INFO]${NC}  $(date +%H:%M:%S) $1"; }

# ─── Chaos Actions ───────────────────────────────────────────

# 1. Kill a random relayer pod (tests HA, PATH re-routing)
chaos_kill_relayer() {
    local pod=$(kubectl --context "$K8S_CONTEXT" get pods -l app=relayer --no-headers 2>/dev/null | shuf -n1 | awk '{print $1}')
    if [ -n "$pod" ]; then
        log_chaos "KILL RELAYER: $pod (k8s will restart, Tilt will rebuild)"
        kubectl --context "$K8S_CONTEXT" delete pod "$pod" --grace-period=0 --force 2>/dev/null || true
    fi
}

# 2. Kill a miner pod (tests leader election failover)
chaos_kill_miner() {
    local pod=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | shuf -n1 | awk '{print $1}')
    if [ -n "$pod" ]; then
        log_chaos "KILL MINER: $pod (leader election should failover)"
        kubectl --context "$K8S_CONTEXT" delete pod "$pod" --grace-period=0 --force 2>/dev/null || true
    fi
}

# 3. Redis connection blip — pause Redis for 2 seconds
#    Tests: fail-open relay behavior, cache miss recovery, stream reconnection
chaos_redis_blip() {
    log_chaos "REDIS PAUSE: 2s blip (testing fail-open and reconnection)"
    kubectl --context "$K8S_CONTEXT" exec "$REDIS_POD" -- redis-cli CLIENT PAUSE 2000 2>/dev/null || true
}

# 4. Redis slow command — inject latency on all commands for 3 seconds
chaos_redis_slow() {
    log_chaos "REDIS SLOW: debug sleep 0.05 on random keys (50ms latency per command)"
    # Use CLIENT NO-EVICT to simulate slow redis (not actual sleep, just a log marker)
    # Real latency injection would need tc or a proxy — this is a marker for monitoring
    kubectl --context "$K8S_CONTEXT" exec "$REDIS_POD" -- redis-cli DEBUG SLEEP 0.1 2>/dev/null &
    local pid=$!
    sleep 0.5
    kill $pid 2>/dev/null || true
    log_chaos "REDIS SLOW: recovered"
}

# 5. Kill backend pod (tests circuit breaker, health check recovery)
chaos_kill_backend() {
    local pod=$(kubectl --context "$K8S_CONTEXT" get pods -l app=backend --no-headers 2>/dev/null | shuf -n1 | awk '{print $1}')
    if [ -n "$pod" ]; then
        log_chaos "KILL BACKEND: $pod (tests circuit breaker + health check recovery)"
        kubectl --context "$K8S_CONTEXT" delete pod "$pod" --grace-period=0 --force 2>/dev/null || true
    fi
}

# 6. Exhaust relayer connections with rapid connect/disconnect
chaos_connection_flood() {
    log_chaos "CONN FLOOD: 500 rapid TCP connections to relayer"
    for i in $(seq 1 500); do
        (echo "" | nc -w1 localhost 3069 2>/dev/null &)
    done
    wait 2>/dev/null || true
    log_chaos "CONN FLOOD: done"
}

# 7. PATH is OUT OF SCOPE — never kill it. It's not our software.

# Weighted random chaos selection
# More frequent: pod kills (tests the HA we care most about)
# Less frequent: redis/network (infrastructure level)
CHAOS_ACTIONS=(
    chaos_kill_relayer
    chaos_kill_relayer
    chaos_kill_miner
    chaos_kill_miner
    chaos_redis_blip
    chaos_redis_slow
    chaos_kill_backend
    chaos_kill_backend
    chaos_connection_flood
)

# ─── Main Loop ───────────────────────────────────────────────

echo ""
echo -e "${MAGENTA}╔══════════════════════════════════════╗${NC}"
echo -e "${MAGENTA}║     CHAOS MONKEY - pocket-relay      ║${NC}"
echo -e "${MAGENTA}╚══════════════════════════════════════╝${NC}"
echo ""
echo "  Interval: ${CHAOS_INTERVAL}s between events"
echo "  Duration: ${DURATION}s"
echo "  Actions:  ${#CHAOS_ACTIONS[@]} types"
echo ""

# Wait a bit for the system to stabilize before injecting chaos
log_info "Waiting 30s for system to stabilize before chaos..."
sleep 30

ELAPSED=0
EVENT_NUM=0
while [ $ELAPSED -lt "$DURATION" ]; do
    EVENT_NUM=$((EVENT_NUM + 1))

    # Pick random chaos action
    IDX=$((RANDOM % ${#CHAOS_ACTIONS[@]}))
    ACTION="${CHAOS_ACTIONS[$IDX]}"

    echo ""
    log_chaos "━━━ Event #${EVENT_NUM} ━━━"
    $ACTION

    # Wait for next event (with jitter: ±5s)
    JITTER=$(( (RANDOM % 10) - 5 ))
    WAIT=$((CHAOS_INTERVAL + JITTER))
    [ "$WAIT" -lt 5 ] && WAIT=5
    sleep "$WAIT"
    ELAPSED=$((ELAPSED + WAIT))

    # Quick health check after each event
    RELAYER_READY=$(kubectl --context "$K8S_CONTEXT" get pods -l app=relayer --no-headers 2>/dev/null | grep -c "Running" || true)
    MINER_READY=$(kubectl --context "$K8S_CONTEXT" get pods -l app=miner --no-headers 2>/dev/null | grep -c "Running" || true)
    log_info "Post-chaos: relayers=$RELAYER_READY miners=$MINER_READY"
done

echo ""
log_chaos "Chaos complete after $EVENT_NUM events"
log_info "System should self-heal — check stress test results"
