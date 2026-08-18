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
    --relays | --concurrency | --service | --timeout-min)
        # Guard the shift: with a missing value, `shift 2` on one remaining
        # argument fails silently under `set -u` without -e and the loop
        # re-reads the same flag forever at 100% CPU.
        if [ $# -lt 2 ]; then
            printf '%s requires a value\n' "$1" >&2
            exit 2
        fi
        case "$1" in
        --relays) RELAYS="$2" ;;
        --concurrency) CONCURRENCY="$2" ;;
        --service) SERVICE_ID="$2" ;;
        --timeout-min) SETTLE_TIMEOUT_MIN="$2" ;;
        esac
        shift 2
        ;;
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

# Every transport the relayer serves, each against its staked localnet service.
# One transport per line: a change can break one routing path while the others
# stay green, or -- worse -- serve a transport's relays without publishing them
# to the WAL, so they are never mined or paid. Both failure modes are invisible
# to a single-service run. The matrix also covers both validation modes for
# free: develop-http runs optimistic, every other service runs eager, so a
# mode-specific regression shows up as exactly one column failing.
#
# Load shapes per transport:
#   jsonrpc/websocket/grpc  --load-test (concurrent, end-to-end verified)
#   stream                  --batches (load-test unsupported). Billing model
#                           pinned live 2026-08-18: one stream REQUEST bills ONE
#                           relay -- the SMST leaf is the signed request, so the
#                           batch count does not multiply billing.
#   cometbft                sequential single relays; one request bills one relay.
run_transport_load() {
    local mode="$1" service="$2" out rc
    case "$mode" in
    jsonrpc | websocket | grpc)
        out="$("$BIN" relay "$mode" --localnet --service "$service" \
            --relayer-url "$relayer_url" \
            --load-test -n "$RELAYS_PER_TRANSPORT" --concurrency "$CONCURRENCY" \
            --all-suppliers 2>&1)"
        rc=$?
        ;;
    stream | cometbft)
        # Sequential single requests; each successful invocation is one billed
        # relay. --all-suppliers does not apply (stream pins its supplier at
        # the handshake), so successive requests spread suppliers on their own.
        out=""
        rc=0
        TRANSPORT_SENT=0
        local i
        for i in 1 2 3; do
            local one
            if [ "$mode" = "stream" ]; then
                one="$("$BIN" relay stream --localnet --service "$service" \
                    --relayer-url "$relayer_url" --batches 3 2>&1)" || rc=$?
            else
                one="$("$BIN" relay cometbft --localnet --service "$service" \
                    --relayer-url "$relayer_url" 2>&1)" || rc=$?
            fi
            out="${out}${one}"$'\n'
            [ "$rc" -ne 0 ] && break
            TRANSPORT_SENT=$((TRANSPORT_SENT + 1))
        done
        ;;
    esac
    TRANSPORT_OUT="$out"
    return "$rc"
}

transport_success_count() {
    local mode="$1" out="$2"
    case "$mode" in
    jsonrpc | websocket | grpc)
        printf '%s\n' "$out" | awk -F': *' '/^Successful:/ {print $2; exit}'
        ;;
    stream | cometbft)
        # Counted by the runner: one successful invocation = one billed relay.
        printf '%s' "${TRANSPORT_SENT:-0}"
        ;;
    esac
}

# mode:service pairs. Override with MATRIX="jsonrpc:develop-http ..." to narrow.
MATRIX="${MATRIX:-jsonrpc:develop-http websocket:develop-websocket grpc:develop-grpc stream:develop-stream cometbft:develop-cometbft}"
RELAYS_PER_TRANSPORT="${RELAYS_PER_TRANSPORT:-60}"

# Issue #25: single-relay-per-supplier claims can vanish between the WAL and
# the claim (a supplier's entire 1-relay session goes unclaimed, with no drop
# counter and no failure state). Reproduced at 1 relay/supplier regardless of
# session timing; never observed at >=4 relays/supplier (300/300 billed). Until
# it is fixed, warn when the load is thin enough to trip it -- the exact
# served==billed assertion is then testing #25, not the change under test.
if [ "$RELAYS_PER_TRANSPORT" -lt $(( supplier_count * 2 )) ]; then
    gate_skip "RELAYS_PER_TRANSPORT=${RELAYS_PER_TRANSPORT} gives <2 relays per supplier (${supplier_count} suppliers): exact billing may trip issue #25"
fi

# Height at which THIS run's load begins: settlement events are later filtered
# to sessions ending at or after it, so claims from earlier traffic on a shared
# localnet (previous runs, bursts) cannot satisfy this run's expectations.
load_start_height="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
    jq -r '.result.sync_info.latest_block_height // "0"')"

# Per-service expectation ledger: "mode service sent exact" per line. `exact`
# marks transports whose accounting model is pinned (one request signs one
# fresh relay, so served == billed). stream and cometbft are asserted as >=1
# proven and REPORTED, to pin their model empirically before demanding it.
matrix_ledger="${BIN_DIR}/matrix.tsv"
: >"$matrix_ledger"

loaded_services=""
for pair in $MATRIX; do
    mode="${pair%%:*}"
    service="${pair#*:}"

    gate_step "load: ${mode} -> ${service} via ${relayer_url}"

    if run_transport_load "$mode" "$service"; then
        succeeded="$(transport_success_count "$mode" "$TRANSPORT_OUT")"
        if [ "${succeeded:-0}" -gt 0 ]; then
            gate_pass "${mode}: ${succeeded} relay(s)/batch(es) verified end to end"
            loaded_services="${loaded_services} ${service}"
            # Every transport's model is pinned to one request = one billed
            # relay (stream/cometbft verified live 2026-08-18), so all rows
            # get the exact served==billed assertion.
            printf '%s\t%s\t%s\t1\n' "$mode" "$service" "$succeeded" >>"$matrix_ledger"
        else
            gate_fail "${mode}: the run exited clean but nothing was verified:"
            gate_detail "$(printf '%s\n' "$TRANSPORT_OUT" | tail -15)"
        fi
    else
        gate_fail "${mode}: the load run failed:"
        gate_detail "$(printf '%s\n' "$TRANSPORT_OUT" | tail -15)"
    fi
done

if [ -z "$loaded_services" ]; then
    gate_fail "no transport delivered a single relay"
    gate_verdict "live"
fi

# ---------------------------------------------------------------------------
gate_step "settle: waiting for FINAL on-chain outcomes per service (up to ${SETTLE_TIMEOUT_MIN} min)"

# What counts as proof that a relay earned money is the SETTLEMENT, not the
# inclusion of the claim: a claim can land on-chain and still expire without
# its proof, be discarded, or get its supplier slashed. This reads the terminal
# events the chain emits in its EndBlocker via block_results, and it does so
# directly against the validator -- the miner's settlement monitor is disabled
# by default, so its metrics are empty on a stock localnet.
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

supplier_filter="$(printf '%s\n' "$suppliers" | paste -sd'|' -)"

deadline=$(( $(date +%s) + SETTLE_TIMEOUT_MIN * 60 ))
events_file="${BIN_DIR}/settlement_events.jsonl"
: >"$events_file"
resolved=0

# The billing assertion is per service: every service the matrix loaded must
# produce at least one claim settled as PROVEN. "Overall something settled" is
# how a dead transport hides behind a healthy one.
# billed_relays <svc> -- proven relays settled for the service, counting only
# sessions that ended at or after this run's load started.
billed_relays() {
    # A settled claim is PAID with status 0 (PENDING_VALIDATION: the protocol
    # did not require a proof for this claim) as well as 1 (VALIDATED). Only
    # 2 (INVALID) is a bad settlement. Verified against poktroll v0.1.35
    # x/proof/types: the enum has exactly those three values -- there is no 3.
    jq -rs --arg svc "$1" --argjson minend "${load_start_height:-0}" '
        [.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
             | select((.attrs.service_id // "" | gsub("\"";"")) == $svc)
             | select(.attrs.claim_proof_status_int // "" | tostring | test("^\"?[01]\"?$"))
             | select((.attrs.session_end_block_height // "0" | tostring | gsub("[^0-9]";"") | tonumber) >= $minend)]
        | [length, ([.[].attrs.num_relays // "0" | tostring | gsub("[^0-9]";"") | tonumber] | add // 0)]
        | @tsv' "$events_file" 2>/dev/null || printf '0\t0'
}

services_pending() {
    local missing=""
    while IFS=$'\t' read -r mode svc sent exact; do
        [ -z "$svc" ] && continue
        local proven_n relays_n
        read -r proven_n relays_n <<<"$(billed_relays "$svc")"
        if [ "$exact" = "1" ]; then
            [ "${relays_n:-0}" -lt "${sent:-0}" ] && missing="${missing} ${svc}(${relays_n:-0}/${sent})"
        else
            [ "${proven_n:-0}" -eq 0 ] && missing="${missing} ${svc}"
        fi
    done <"$matrix_ledger"
    printf '%s' "$missing"
}

while [ "$(date +%s)" -lt "$deadline" ]; do
    height_now="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
        jq -r '.result.sync_info.latest_block_height // empty')"
    [ -z "$height_now" ] && { sleep "$POLL_INTERVAL_S"; continue; }

    scan_settlement_events "$height_before" "$height_now" \
        | grep -E "$supplier_filter" >"$events_file" || true

    missing="$(services_pending)"
    if [ -z "$missing" ]; then
        resolved=1
        break
    fi

    printf '           height %s, still waiting for proven claims on:%s\n' "$height_now" "$missing"
    sleep "$POLL_INTERVAL_S"
done

# ---------------------------------------------------------------------------
gate_step "assert: final settlement outcome, per service"

while IFS=$'\t' read -r mode svc sent exact; do
    [ -z "$svc" ] && continue
    read -r proven_n relays_n <<<"$(billed_relays "$svc")"

    if [ "$exact" = "1" ]; then
        # The accounting model for this transport is one request = one billed
        # relay (fresh ring signature per request, so no dedup collapse).
        # Anything less than equality is silent partial loss: relays served to
        # clients that never reached a claim.
        if [ "${relays_n:-0}" -eq "${sent:-0}" ]; then
            gate_pass "${svc} (${mode}): ${sent}/${sent} relays billed across ${proven_n} proven claim(s)"
        elif [ "${relays_n:-0}" -gt "${sent:-0}" ]; then
            gate_fail "${svc} (${mode}): billed MORE than sent (${relays_n}/${sent}) -- foreign traffic or double count"
        else
            gate_fail "${svc} (${mode}): served ${sent}, billed only ${relays_n:-0} -- $((sent - ${relays_n:-0})) relay(s) LOST between serve and claim"
            printf '         check the WAL (redis streams) and submissions for this service\n'
        fi
    else
        if [ "${proven_n:-0}" -gt 0 ] && [ "${relays_n:-0}" -gt 0 ]; then
            gate_pass "${svc} (${mode}): ${proven_n} claim(s) PROVEN, sent=${sent} billed=${relays_n} (model unpinned: reported, not asserted)"
        else
            gate_fail "${svc} (${mode}): served relays but NONE were billed (proven=${proven_n:-0})"
        fi
    fi
done <"$matrix_ledger"

expired="$(grep -c 'EventClaimExpired' "$events_file" 2>/dev/null || true)"
settled_invalid="$(jq -rs '[.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
    | select(.attrs.claim_proof_status_int // "" | tostring | test("^\"?2\"?$"))] | length' \
    "$events_file" 2>/dev/null || echo 0)"
slashed="$(grep -c 'EventSupplierSlashed' "$events_file" 2>/dev/null || true)"
discarded="$(grep -c 'EventClaimDiscarded' "$events_file" 2>/dev/null || true)"

total_expired=$(( ${settled_invalid:-0} + ${expired:-0} ))
if [ "$total_expired" -eq 0 ]; then
    gate_pass "no claim expired"
else
    gate_fail "${total_expired} claim(s) EXPIRED -- relays served and never paid"
    jq -rs '.[] | select(.type == "pocket.tokenomics.EventClaimExpired")
            | "           service=\(.attrs.service_id // "?") reason=\(.attrs.expiration_reason // "?") relays=\(.attrs.num_relays // "?")"' \
        "$events_file" 2>/dev/null | head -10
fi

if [ "${slashed:-0}" -eq 0 ]; then
    gate_pass "no supplier slashed"
else
    gate_fail "${slashed} SLASHING event(s) -- staked funds burned"
fi
[ "${discarded:-0}" -ne 0 ] && gate_fail "${discarded} claim(s) discarded without settling"

# The miner's own terminal failure states, as corroboration: they should agree
# with the chain, and a disagreement is itself the finding.
for state in claim_missing claim_tx_error proof_tx_error proof_window_closed claim_window_closed; do
    n=0
    for supplier in $suppliers; do
        n=$(( n + $("$BIN" redis sessions --supplier "$supplier" --state "$state" 2>/dev/null |
            grep -c '^session' || true) ))
    done
    [ "$n" -gt 0 ] && gate_fail "miner reports ${n} session(s) in failure state '${state}'"
done

if [ "$resolved" -eq 0 ]; then
    gate_fail "timed out after ${SETTLE_TIMEOUT_MIN} min with services still unpaid"
fi

gate_verdict "live"
