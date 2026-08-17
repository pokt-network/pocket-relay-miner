#!/usr/bin/env bash
#
# Gate: live validation on the Tilt localnet. Tens of minutes, needs a cluster.
#
# Proves the money path end to end: relays are served and signed, the miner
# consumes them off the WAL, and the resulting claim -- and its proof, when one
# is required -- are INCLUDED ON-CHAIN. Nothing below infers success from a
# Redis key or an HTTP status.
#
# Usage:
#   scripts/gates/live.sh                     # verify, load, wait, assert
#   scripts/gates/live.sh --preflight-only    # check readiness and stop
#   scripts/gates/live.sh --relays 600 --concurrency 10
#   scripts/gates/live.sh --service develop-grpc
#
# This gate NEVER starts or stops anything. If the localnet is not up it says
# what to run and exits non-zero. Bringing the cluster up takes ports and
# containers that another session on this machine may be using, so that
# decision stays with the person at the keyboard.
#
# Three things that will otherwise waste an afternoon, all learned the hard way:
#
#   * Load goes through the relay CLI at :8180, NEVER the PATH gateway. PATH
#     answers a relayer 503 with 200 and an empty body, so a gateway-side run
#     reports 20000/20000 OK with an empty WAL.
#   * There is an economic cap of roughly 115-130 mined relays per supplier per
#     session (~109s). Past it the relayer correctly returns 429 and only the
#     first session claims, which looks like a broken pipeline and is not. The
#     defaults here stay under it; --all-suppliers spreads the rest.
#   * Tilt's port-forward is sometimes IPv6-only. curl to localhost still works
#     while the Go CLI fails with "connect: connection refused" and the load
#     test reports 0%. Detected in preflight and worked around.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

# ---------------------------------------------------------------------------
# Configuration
RELAYS="${RELAYS:-600}"
CONCURRENCY="${CONCURRENCY:-10}"
SERVICE_ID="${SERVICE_ID:-develop-http}"
RELAYER_PORT="${RELAYER_PORT:-8180}"
VALIDATOR_RPC="${VALIDATOR_RPC:-http://localhost:26657}"
# How long to wait for the claim and proof windows to close and the inclusion
# reconciler to resolve. Localnet sessions are 10 blocks with 8-block claim and
# proof windows; polling rather than sleeping means this is an upper bound, not
# a fixed cost.
SETTLE_TIMEOUT_MIN="${SETTLE_TIMEOUT_MIN:-25}"
POLL_INTERVAL_S="${POLL_INTERVAL_S:-15}"

preflight_only=0
while [ $# -gt 0 ]; do
    case "$1" in
    --preflight-only) preflight_only=1; shift ;;
    --relays) RELAYS="${2:-}"; shift 2 ;;
    --concurrency) CONCURRENCY="${2:-}"; shift 2 ;;
    --service) SERVICE_ID="${2:-}"; shift 2 ;;
    --timeout-min) SETTLE_TIMEOUT_MIN="${2:-}"; shift 2 ;;
    -h | --help)
        sed -n '2,32p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *) printf 'unknown argument: %s\n' "$1" >&2; exit 2 ;;
    esac
done

# ---------------------------------------------------------------------------
gate_step "preflight: tooling"

for tool in kubectl jq curl go; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        gate_fail "$tool is not installed"
    fi
done
[ "$gate_failed" -ne 0 ] && gate_verdict "live"
gate_pass "kubectl, jq, curl, go present"

# ---------------------------------------------------------------------------
gate_step "preflight: cluster"

# The localnet runs in kind. Any other context is either a mistake or, worse,
# a real cluster -- this gate sends load and must never be pointed at one.
current_ctx="$(kubectl config current-context 2>/dev/null || true)"
case "$current_ctx" in
kind-*)
    gate_pass "kubectl context is $current_ctx"
    ;;
"")
    gate_fail "no kubectl context is set"
    ;;
*)
    gate_fail "kubectl context is '$current_ctx', which is not a kind cluster"
    printf '         this gate sends load; refusing to run outside kind\n'
    ;;
esac
[ "$gate_failed" -ne 0 ] && gate_verdict "live"

pods="$(kubectl get pods --no-headers 2>/dev/null || true)"
if [ -z "$pods" ]; then
    gate_fail "no pods in the current namespace -- the localnet is not up"
    printf '         bring it up yourself with: %stilt up -f Tiltfile --stream%s\n' \
        "$GATE_BOLD" "$GATE_RESET"
    printf '         (this gate does not start anything: the cluster takes ports\n'
    printf '          and containers another session on this machine may be using)\n'
    gate_verdict "live"
fi

for app in relayer miner validator; do
    running="$(printf '%s\n' "$pods" | awk -v a="$app" '$1 ~ a && $3 == "Running" {n++} END {print n+0}')"
    if [ "$running" -gt 0 ]; then
        gate_pass "$app: $running pod(s) Running"
    else
        gate_fail "$app: no Running pod"
    fi
done
[ "$gate_failed" -ne 0 ] && gate_verdict "live"

# ---------------------------------------------------------------------------
gate_step "preflight: endpoints"

# Tilt's port-forward sometimes binds IPv6 only. curl to "localhost" resolves
# to ::1 and succeeds while the Go CLI dials 127.0.0.1 and fails, which shows
# up as a load test that reports 0% and no other symptom.
relayer_url="http://localhost:${RELAYER_PORT}"
if command -v ss >/dev/null 2>&1; then
    listeners="$(ss -lntH "sport = :${RELAYER_PORT}" 2>/dev/null || true)"
    if [ -n "$listeners" ] && ! printf '%s\n' "$listeners" | grep -qE '(0\.0\.0\.0|127\.0\.0\.1):'"${RELAYER_PORT}"; then
        relayer_url="http://[::1]:${RELAYER_PORT}"
        gate_skip "port ${RELAYER_PORT} is IPv6-only; using ${relayer_url}"
    fi
fi

if curl -fsS --max-time 5 "${relayer_url}/health" >/dev/null 2>&1; then
    gate_pass "relayer healthy at ${relayer_url}"
else
    gate_fail "relayer does not answer /health at ${relayer_url}"
fi

height_before="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
    jq -r '.result.sync_info.latest_block_height // empty')"
if [ -n "$height_before" ]; then
    gate_pass "chain at height ${height_before}"
else
    gate_fail "validator RPC ${VALIDATOR_RPC} did not report a height"
fi
[ "$gate_failed" -ne 0 ] && gate_verdict "live"

# A chain that is up but not producing blocks never closes a claim window, so
# the run would burn the whole timeout and report a false negative.
sleep 3
height_after="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
    jq -r '.result.sync_info.latest_block_height // empty')"
if [ "${height_after:-0}" -gt "${height_before:-0}" ] 2>/dev/null; then
    gate_pass "chain is producing blocks (${height_before} -> ${height_after})"
else
    gate_skip "no new block in 3s -- may be mid-block, continuing"
fi

# ---------------------------------------------------------------------------
gate_step "preflight: binary"

BIN_DIR="$(mktemp -d)"
trap 'rm -rf "$BIN_DIR"' EXIT
BIN="${BIN_DIR}/pocket-relay-miner"
if build_out="$(go build -o "$BIN" . 2>&1)"; then
    gate_pass "built the CLI under test"
else
    gate_fail "could not build the CLI:"
    gate_detail "$build_out"
    gate_verdict "live"
fi

suppliers="$("$BIN" redis supplier --list 2>/dev/null | awk '/^pokt/{print $1}')"
supplier_count="$(printf '%s\n' "$suppliers" | grep -c '^pokt' || true)"
if [ "$supplier_count" -gt 0 ]; then
    gate_pass "${supplier_count} supplier(s) registered"
else
    gate_fail "no suppliers in the registry -- the miner has not registered any"
fi
[ "$gate_failed" -ne 0 ] && gate_verdict "live"

if [ "$preflight_only" -eq 1 ]; then
    printf '\n%spreflight only%s -- the localnet is ready for a live run\n' \
        "$GATE_BOLD" "$GATE_RESET"
    gate_verdict "live (preflight)"
fi

# ---------------------------------------------------------------------------
gate_step "load: ${RELAYS} relays over ${SERVICE_ID} via the CLI at ${relayer_url}"

load_out="$("$BIN" relay jsonrpc --localnet --service "$SERVICE_ID" \
    --relayer-url "$relayer_url" \
    --load-test -n "$RELAYS" --concurrency "$CONCURRENCY" --all-suppliers 2>&1)"
load_rc=$?

printf '%s\n' "$load_out" | tail -20 | sed 's/^/           /'

if [ "$load_rc" -ne 0 ]; then
    gate_fail "the load run exited non-zero"
    gate_verdict "live"
fi

# The CLI verifies each relay end to end (supplier signature plus the backend's
# own error field), so its success count is the relayer's true behaviour --
# unlike a status-code count through the gateway.
succeeded="$(printf '%s\n' "$load_out" | awk -F': *' '/^Successful:/ {print $2; exit}')"
errored="$(printf '%s\n' "$load_out" | awk -F': *' '/^Errors:/ {print $2; exit}')"

if [ "${succeeded:-0}" -gt 0 ]; then
    gate_pass "${succeeded} relay(s) verified end to end, ${errored:-?} error(s)"
else
    gate_fail "no relay succeeded -- see the output above"
    gate_verdict "live"
fi


# ---------------------------------------------------------------------------
gate_step "settle: waiting for a FINAL on-chain outcome (up to ${SETTLE_TIMEOUT_MIN} min)"

# What counts as proof that a relay earned money is the SETTLEMENT, not the
# inclusion of the claim. A claim can land on-chain and still expire without its
# proof, be discarded, or get its supplier slashed -- all after inclusion. So
# this reads the terminal events the chain emits in its EndBlocker:
#
#   EventClaimSettled  claim_proof_status_int 1=PROVEN (paid), 3=EXPIRED
#   EventClaimExpired  expiration_reason PROOF_MISSING / PROOF_INVALID
#   EventSupplierSlashed   stake burned -- the worst outcome there is
#   EventClaimDiscarded    dropped without settling
#
# These come from block_results (FinalizeBlockEvents), queried directly against
# the validator. Deliberately NOT read from the miner's settlement metrics: the
# settlement monitor is disabled by default (config.miner.example.yaml leaves the
# whole block commented, and Tilt renders its config from that file), so those
# series are empty on a stock localnet and asserting on them would assert on
# nothing.
scan_settlement_events() {
    local from="$1" to="$2"
    local h
    for ((h = from; h <= to; h++)); do
        curl -fsS --max-time 10 "${VALIDATOR_RPC}/block_results?height=${h}" 2>/dev/null |
            jq -c --arg h "$h" '
                (.result.finalize_block_events // [])[]
                | select(.type | startswith("pocket.tokenomics.Event"))
                | {height: $h, type: .type,
                   attrs: (.attributes // [] | map({(.key): .value}) | add // {})}
            ' 2>/dev/null || true
    done
}

# Filter to the suppliers this run drove; another session's traffic on a shared
# localnet must not turn this green.
supplier_filter="$(printf '%s\n' "$suppliers" | paste -sd'|' -)"

deadline=$(( $(date +%s) + SETTLE_TIMEOUT_MIN * 60 ))
events_file="${BIN_DIR}/settlement_events.jsonl"
: >"$events_file"
resolved=0

while [ "$(date +%s)" -lt "$deadline" ]; do
    height_now="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
        jq -r '.result.sync_info.latest_block_height // empty')"
    [ -z "$height_now" ] && { sleep "$POLL_INTERVAL_S"; continue; }

    scan_settlement_events "$height_before" "$height_now" \
        | grep -E "$supplier_filter" >"$events_file" || true

    settled_count="$(grep -c 'EventClaimSettled' "$events_file" || true)"
    expired_count="$(grep -cE 'EventClaimExpired' "$events_file" || true)"

    if [ "$(( settled_count + expired_count ))" -gt 0 ]; then
        resolved=1
        break
    fi

    printf '           height %s, no terminal settlement event yet\n' "$height_now"
    sleep "$POLL_INTERVAL_S"
done

# ---------------------------------------------------------------------------
gate_step "assert: final settlement outcome"

count_events() { grep -c "$1" "$events_file" 2>/dev/null || true; }

proven="$(jq -rs '[.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
                        | select(.attrs.claim_proof_status_int // "" | tostring | test("^\"?1\"?$"))] | length' \
    "$events_file" 2>/dev/null || echo 0)"
settled_expired="$(jq -rs '[.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
                        | select(.attrs.claim_proof_status_int // "" | tostring | test("^\"?3\"?$"))] | length' \
    "$events_file" 2>/dev/null || echo 0)"
expired="$(count_events 'EventClaimExpired')"
slashed="$(count_events 'EventSupplierSlashed')"
discarded="$(count_events 'EventClaimDiscarded')"

if [ "${proven:-0}" -gt 0 ]; then
    gate_pass "${proven} claim(s) SETTLED AS PROVEN -- relays were paid"
else
    gate_fail "no claim settled as proven: nothing earned"
fi

# An expiry is the expensive failure: the work was done, claimed, and then lost.
total_expired=$(( ${settled_expired:-0} + ${expired:-0} ))
if [ "$total_expired" -eq 0 ]; then
    gate_pass "no claim expired"
else
    gate_fail "${total_expired} claim(s) EXPIRED -- relays served and never paid"
    jq -rs '.[] | select(.type == "pocket.tokenomics.EventClaimExpired")
            | "           reason=\(.attrs.expiration_reason // "?") relays=\(.attrs.num_relays // "?") height=\(.height)"' \
        "$events_file" 2>/dev/null | head -10
fi

if [ "${slashed:-0}" -eq 0 ]; then
    gate_pass "no supplier slashed"
else
    gate_fail "${slashed} SLASHING event(s) -- staked funds burned"
    jq -rs '.[] | select(.type == "pocket.tokenomics.EventSupplierSlashed")
            | "           \(.attrs)"' "$events_file" 2>/dev/null | head -5
fi

if [ "${discarded:-0}" -ne 0 ]; then
    gate_fail "${discarded} claim(s) discarded without settling"
fi

# Earning zero while settling as proven means the reward arithmetic is wrong,
# which a status check alone would not catch.
earned="$(jq -rs '[.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
                       | .attrs.reward_distribution // ""] | length' "$events_file" 2>/dev/null || echo 0)"
if [ "${proven:-0}" -gt 0 ] && [ "${earned:-0}" -eq 0 ]; then
    gate_fail "claims settled as proven but no reward distribution was emitted"
fi

# The miner's own terminal states, as corroboration: they should agree with the
# chain, and a disagreement is itself the finding.
for state in claim_missing claim_tx_error proof_tx_error proof_window_closed claim_window_closed; do
    n=0
    for supplier in $suppliers; do
        n=$(( n + $("$BIN" redis sessions --supplier "$supplier" --state "$state" 2>/dev/null |
            grep -c '^session' || true) ))
    done
    [ "$n" -gt 0 ] && gate_fail "miner reports ${n} session(s) in failure state '${state}'"
done

if [ "$resolved" -eq 0 ]; then
    gate_fail "timed out after ${SETTLE_TIMEOUT_MIN} min with no terminal settlement event"
    printf '         nothing settled is NOT a pass: the run proved nothing about payment\n'
fi

gate_verdict "live"
