#!/usr/bin/env bash
#
# Chaos Monkey for pocket-relay-miner
#
# Runs alongside the stress test and randomly injects failures:
#   1. Kill random relayer/miner pods (test HA failover)
#   2. Redis connection blip (test fail-open/closed behavior)
#   3. Redis latency marker (test pool behaviour under slow commands)
#   4. Kill a backend pod (test circuit breaker + health-check recovery)
#   5. Connection flood against the relayer (test pool exhaustion)
#   6. Pull and restore a supplier signing key (test the no_local_signer gate)
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
KEYS_SECRET="supplier-keys"
KEYS_SECRET_FIELD="supplier-keys.yaml"

# Base64 of the untouched supplier-keys.yaml, captured the first time a key is
# pulled and replayed by the EXIT trap. A chaos script that dies holding a key
# hostage turns a test into an outage, so this is snapshotted before the first
# mutation and restored unconditionally, including on Ctrl-C.
KEYS_BACKUP=""

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
    log_chaos "CONN FLOOD: 500 rapid TCP connections to relayer (:8180 — the old code flooded 3069, which is PATH, not our software)"
    for i in $(seq 1 500); do
        (echo "" | nc -w1 localhost 8180 2>/dev/null &)
    done
    wait 2>/dev/null || true
    log_chaos "CONN FLOOD: done"
}

# 6. Pull a supplier signing key, then put it back.
#
# The one failure the other five never produce: the fleet still has the supplier
# in its state (the miner's teardown writes {unstaking, staked: true,
# services: [...]}, which reads as perfectly servable) but can no longer sign for
# it. The relayer must refuse those relays promptly with no_local_signer instead
# of paying for a backend call and failing to sign, and must serve again once the
# key returns.
#
# Two timings matter and neither is instant: kubelet syncs secret volumes on a
# period of roughly a minute, and the key manager only sees the change when that
# lands, so this action is deliberately slower than CHAOS_INTERVAL.
restore_signing_keys() {
    [ -z "$KEYS_BACKUP" ] && return 0

    kubectl --context "$K8S_CONTEXT" patch secret "$KEYS_SECRET" \
        -p "{\"data\":{\"$KEYS_SECRET_FIELD\":\"$KEYS_BACKUP\"}}" >/dev/null 2>&1 || true
    log_info "signing keys restored from snapshot"
    KEYS_BACKUP=""
}

# Restore on every exit path, including a failure under `set -e` and Ctrl-C.
trap restore_signing_keys EXIT

# keys_removed_total prints the fleet-wide count of key removals seen, via
# Prometheus. Prints 0 when it cannot be reached, so an unreachable Prometheus
# shows up as "not registered" rather than as a silent pass.
keys_removed_total() {
    curl -fsS --max-time 5 --get "${PROMETHEUS_URL:-http://localhost:9091}/api/v1/query" \
        --data-urlencode 'query=sum(ha_keys_changes_total{type="removed"})' 2>/dev/null |
        jq -r '[.data.result[]?.value[1] | tonumber] | add // 0' 2>/dev/null | cut -d. -f1 || echo 0
}

# secret_key_count prints how many keys the secret currently carries.
secret_key_count() {
    kubectl --context "$K8S_CONTEXT" get secret "$KEYS_SECRET" \
        -o "jsonpath={.data['supplier-keys\.yaml']}" 2>/dev/null \
        | base64 -d 2>/dev/null | grep -c '^[[:space:]]*-' || true
}

chaos_pull_signing_key() {
    # Snapshot once. If a previous event is still holding a key out, skip rather
    # than snapshotting the already-reduced secret as the "original".
    if [ -n "$KEYS_BACKUP" ]; then
        log_chaos "PULL KEY: skipped, a key is already pulled"
        return 0
    fi

    local original
    original=$(kubectl --context "$K8S_CONTEXT" get secret "$KEYS_SECRET" \
        -o "jsonpath={.data['supplier-keys\.yaml']}" 2>/dev/null || true)
    if [ -z "$original" ]; then
        log_chaos "PULL KEY: skipped, secret $KEYS_SECRET has no $KEYS_SECRET_FIELD"
        return 0
    fi

    local before
    before=$(secret_key_count)
    # A failed kubectl gives an empty string, and an empty string in an integer
    # test aborts the whole script under `set -e`.
    [ -z "$before" ] && before=0
    if [ "$before" -lt 2 ]; then
        log_chaos "PULL KEY: skipped, only $before key(s) — pulling the last one is an outage, not chaos"
        return 0
    fi

    KEYS_BACKUP="$original"

    # Drop the last list entry. The secret is `keys:` followed by one `- <hex>`
    # per key (Tiltfile builds it with encode_yaml), so deleting the final list
    # line removes exactly one key and leaves valid YAML.
    local reduced
    reduced=$(printf '%s' "$original" | base64 -d | sed -e '${/^[[:space:]]*-/d}' | base64 -w0)

    log_chaos "PULL KEY: removing 1 of $before supplier keys (relays for it must get no_local_signer, not a signing error)"
    if ! kubectl --context "$K8S_CONTEXT" patch secret "$KEYS_SECRET" \
        -p "{\"data\":{\"$KEYS_SECRET_FIELD\":\"$reduced\"}}" >/dev/null 2>&1; then
        log_chaos "PULL KEY: patch failed, nothing changed"
        KEYS_BACKUP=""
        return 0
    fi

    # Wait for the fleet to notice, measured on the COUNTER rather than by
    # grepping the miner log. The log version was wrong in both directions: it
    # matched a "drain decision audit" line left by the PREVIOUS event (it looks
    # at a 3-minute window) and reported a detection that had not happened, and it
    # missed real ones when the line fell off the tail. Measured 2026-08-21 while
    # it reported 0 detections in 4 events: ha_keys_changes_total{type="removed"}
    # was 13 and the miner had logged 16 audits.
    # Deliberately NOT named before/after: `before` already holds the key count
    # this function restores against, and a second `local before` in the same
    # function silently overwrites it -- which turned the recovery check into a
    # comparison against the removals metric and raised a false "the fleet is
    # short a key" while the secret was in fact whole.
    local removed_at_start removed_now waited=0
    removed_at_start="$(keys_removed_total)"
    while [ "$waited" -lt 120 ]; do
        removed_now="$(keys_removed_total)"
        if [ "${removed_now:-0}" -gt "${removed_at_start:-0}" ]; then
            log_chaos "PULL KEY: fleet registered the removal after ${waited}s"
            break
        fi
        sleep 10
        waited=$((waited + 10))
    done
    [ "$waited" -ge 120 ] && log_chaos "PULL KEY: fleet did not register a key removal within 120s — CHECK THIS"

    # Hold the key out for longer than kubelet's secret sync period, not for one
    # chaos interval. Measured 2026-08-21 with a 20s hold: the fleet registered
    # the removal in 2 of 10 events, because the secret went 17 -> 16 -> 17 well
    # inside the sync window and the pods never saw the middle state. A chaos
    # action that reverts before anything can observe it is a no-op wearing a
    # log line.
    hold="$CHAOS_INTERVAL"
    [ "$hold" -lt 90 ] && hold=90
    sleep "$hold"

    restore_signing_keys

    # Recovery is the half a one-way guard would fail: verify the count is back.
    waited=0
    while [ "$waited" -lt 120 ]; do
        if [ "$(secret_key_count)" -eq "$before" ]; then
            log_chaos "PULL KEY: secret back to $before keys"
            return 0
        fi
        sleep 10
        waited=$((waited + 10))
    done
    log_chaos "PULL KEY: secret did NOT return to $before keys — the fleet is short a key, FIX BEFORE CONTINUING"
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
    chaos_pull_signing_key
)

# CHAOS_ONLY forces a single action instead of sampling. Random selection is the
# point in a soak, but it means a specific action may simply never come up: a
# 300s run at 20s intervals picked 15 events and never chose the signing-key one,
# which would have been reported as "chaos passed" for a change whose whole
# subject is that action. Use it to prove one action works, then go back to random.
if [ -n "${CHAOS_ONLY:-}" ]; then
    CHAOS_ACTIONS=("$CHAOS_ONLY")
fi

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
