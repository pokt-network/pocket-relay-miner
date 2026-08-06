#!/usr/bin/env bash
#
# Preflight for the relay-receipt A/B benchmark.
#
# The benchmark runs on Tilt/localnet, which is sized for development, not for
# sustained load over an hour. Every limit localnet hits — backend saturation,
# meter rejection, Redis pressure, a second relayer replica splitting traffic —
# lands in the same CPU profile as the receipt and cannot be separated
# afterwards. This script establishes capacity BEFORE a single profile is taken,
# and refuses to pass when something would corrupt the measurement.
#
# It changes nothing. Every failure prints what to fix.
#
#   scripts/loadtest/receipt-ab-preflight.sh
#   RPS=400 DURATION=600 scripts/loadtest/receipt-ab-preflight.sh
#
# Run scripts/loadtest/receipt-ab.sh only after this passes.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../lib/common.sh
[ -f "$SCRIPT_DIR/../lib/common.sh" ] && source "$SCRIPT_DIR/../lib/common.sh"

RELAYER_URL="${RELAYER_URL:-http://localhost:8180}"
PPROF_URL="${PPROF_URL:-http://localhost:6060}"
PROM_URL="${PROM_URL:-http://localhost:9091}"
SERVICE_ID="${SERVICE_ID:-develop-http}"
SIM_KEY_ID="${SIM_KEY_ID:-sim-http}"
RPS="${RPS:-400}"
DURATION="${DURATION:-600}"
ARMS="${ARMS:-6}"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'
BOLD=$'\033[1m'; NC=$'\033[0m'

FAILURES=0
WARNINGS=0

say()  { printf '%s==>%s %s\n' "$BOLD" "$NC" "$*"; }
ok()   { printf '  %sok%s   %s\n' "$GREEN" "$NC" "$*"; }
warn() { printf '  %swarn%s %s\n' "$YELLOW" "$NC" "$*"; WARNINGS=$((WARNINGS+1)); }
fail() { printf '  %sFAIL%s %s\n' "$RED" "$NC" "$*"; FAILURES=$((FAILURES+1)); }

# ---------------------------------------------------------------------------
say "1/6  relayer replica count"
# ---------------------------------------------------------------------------
# The one that silently corrupts the result. With more than one replica the
# service splits traffic, but the pprof forward reaches ONE pod. The profile
# would then cover a fraction of the traffic while client-side RPS counts all
# of it: the receipt's percentage share comes out right and every absolute
# number is wrong by the replica count.
REPLICAS="$(kubectl get deploy relayer -o jsonpath='{.spec.replicas}' 2>/dev/null)"
if [ -z "$REPLICAS" ]; then
    fail "relayer deployment not found. Is Tilt up? (tilt up)"
elif [ "$REPLICAS" != "1" ]; then
    fail "relayer has $REPLICAS replicas; the benchmark needs exactly 1.
         Set 'relayer.count: 1' in tilt_config.yaml and let Tilt roll it.
         tilt_config.yaml is TRACKED — revert the change before pushing."
else
    ok "relayer at 1 replica"
    READY="$(kubectl get deploy relayer -o jsonpath='{.status.readyReplicas}' 2>/dev/null)"
    [ "$READY" = "1" ] || fail "relayer replica is not ready (readyReplicas=$READY)"

    RESTARTS="$(kubectl get pods -l app=relayer \
        -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}' 2>/dev/null)"
    if [ "${RESTARTS:-0}" != "0" ]; then
        warn "relayer pod has $RESTARTS restarts; a restart mid-run invalidates the arm"
    fi

    # Recorded so the A/B script can fail if resources change between arms.
    kubectl get deploy relayer \
        -o jsonpath='{.spec.template.spec.containers[0].resources}' 2>/dev/null \
        | tee /tmp/receipt-ab-resources.json >/dev/null
    ok "pod resources recorded to /tmp/receipt-ab-resources.json"
fi

# ---------------------------------------------------------------------------
say "2/6  endpoints reachable"
# ---------------------------------------------------------------------------
probe() {
    local name="$1" url="$2"
    if curl -fsS -m 5 -o /dev/null "$url" 2>/dev/null; then
        ok "$name reachable at $url"
    else
        fail "$name NOT reachable at $url"
    fi
}
probe "pprof"      "$PPROF_URL/debug/pprof/"
probe "prometheus" "$PROM_URL/-/healthy"

# The relayer rejects a GET on the relay path; any HTTP answer proves it is up.
if curl -fsS -m 5 -o /dev/null -w '%{http_code}' "$RELAYER_URL/$SERVICE_ID" >/dev/null 2>&1 \
   || curl -sS -m 5 -o /dev/null "$RELAYER_URL/$SERVICE_ID" 2>/dev/null; then
    ok "relayer answering at $RELAYER_URL"
else
    fail "relayer NOT answering at $RELAYER_URL"
fi

# ---------------------------------------------------------------------------
say "3/6  backend headroom"
# ---------------------------------------------------------------------------
# If the backend saturates before the relayer does, the profile measures
# waiting, not signing. A smaller clean number beats a larger dirty one.
if [ -f "$SCRIPT_DIR/../localonly/loadtest/backends.conf" ] || [ -n "${BACKENDS_CONF:-}" ]; then
    ok "backends.conf present — run 'scripts/loadtest/backends.sh sweep-optimal' and"
    printf '       confirm %s RPS sits WELL below the optimal figure for %s\n' "$RPS" "$SERVICE_ID"
else
    warn "no backends.conf; cannot establish the backend ceiling automatically.
         The Tilt backend must comfortably exceed $RPS RPS or the profile
         measures the backend rather than the receipt."
fi

# ---------------------------------------------------------------------------
say "4/6  meter and stake headroom (real-mode arms only)"
# ---------------------------------------------------------------------------
REAL_ARMS=$((ARMS / 2))
TOTAL_REAL=$((RPS * DURATION * REAL_ARMS))
printf '       %s RPS x %ss x %s real arms = %s relays through the meter\n' \
    "$RPS" "$DURATION" "$REAL_ARMS" "$TOTAL_REAL"
printf '       (simulated arms consume no meter and publish nothing)\n'

REJECTED="$(curl -fsS -m 5 "$PROM_URL/api/v1/query?query=sum(ha_relayer_relays_rejected_total)" 2>/dev/null \
    | grep -o '"value":\[[^]]*\]' | grep -oE '"[0-9.]+"$' | tr -d '"')"
if [ -n "$REJECTED" ]; then
    ok "relays_rejected_total currently $REJECTED — the A/B run must not move it"
else
    warn "could not read ha_relayer_relays_rejected_total; check it manually during the run.
         A meter that starts rejecting mid-arm changes the work per relay and
         destroys comparability between arms."
fi

# ---------------------------------------------------------------------------
say "5/6  session rollover"
# ---------------------------------------------------------------------------
# Rollover changes the work per relay: cache refreshes, claim construction,
# SMST sealing. Acceptable only if EVERY arm spans the same number.
BLOCK_TIME="$(grep -E '^\s*block_time_seconds:' tilt_config.yaml 2>/dev/null | head -1 | grep -oE '[0-9]+')"
if [ -n "$BLOCK_TIME" ]; then
    printf '       block_time_seconds=%s, arm=%ss\n' "$BLOCK_TIME" "$DURATION"
    ok "record how many rollovers each arm spans; every arm must span the same number.
         One arm spanning two and another spanning one is not a valid comparison."
else
    warn "could not read block_time_seconds from tilt_config.yaml"
fi

# ---------------------------------------------------------------------------
say "6/6  Redis headroom (real-mode arms publish to the WAL)"
# ---------------------------------------------------------------------------
if command -v redis-cli >/dev/null 2>&1; then
    USED="$(redis-cli INFO memory 2>/dev/null | grep -E '^used_memory_human:' | tr -d '\r' | cut -d: -f2)"
    MAXM="$(redis-cli INFO memory 2>/dev/null | grep -E '^maxmemory_human:' | tr -d '\r' | cut -d: -f2)"
    if [ -n "$USED" ]; then
        ok "redis used=${USED:-?} max=${MAXM:-unlimited} — record this and re-check after the run"
        printf '       an eviction mid-run invalidates the arm\n'
    else
        warn "redis-cli present but INFO memory returned nothing (is Redis proxied by Tilt?)"
    fi
else
    warn "redis-cli not found; check Redis headroom manually"
fi

# ---------------------------------------------------------------------------
printf '\n'
if [ "$FAILURES" -gt 0 ]; then
    printf '%s%d check(s) failed, %d warning(s). Do NOT run the benchmark.%s\n' \
        "$RED" "$FAILURES" "$WARNINGS" "$NC"
    printf 'A benchmark started on an untuned localnet costs an hour and produces a\n'
    printf 'number that cannot be defended.\n'
    exit 1
fi

printf '%sPreflight passed%s (%d warning(s)).\n' "$GREEN" "$NC" "$WARNINGS"
printf 'Next: scripts/loadtest/receipt-ab.sh\n'
