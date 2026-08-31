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
CONCURRENCY="${CONCURRENCY:-10}"
SERVICE_FILTER="${SERVICE_FILTER:-}"
RELAYER_PORT="${RELAYER_PORT:-8180}"
VALIDATOR_RPC="${VALIDATOR_RPC:-http://localhost:26657}"
# Prometheus, for the ANNOUNCED-drop accounting below. Only used to explain a
# shortfall; if it is unreachable the assertion stays strict, which is the
# safe direction (a real loss must never be excused by a scrape failure).
PROMETHEUS_URL="${PROMETHEUS_URL:-http://localhost:9091}"
# How long to wait for the claim and proof windows to close and the settlement
# to land. The localnet mirrors mainnet block proportions (20-block sessions,
# grace 10, claim +11..+21, proof +22..+32) at 10s blocks, so a session settles
# ~5.5 minutes after it ends; polling rather than sleeping means this is an
# upper bound, not a fixed cost.
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
        # --relays sizes the per-transport load and --service narrows the
        # matrix to every cell of one service. Both map onto the env knobs
        # the script actually consumes -- they used to parse into variables
        # nothing read, so the usage advertised no-ops.
        --relays) RELAYS_PER_TRANSPORT="$2" ;;
        --concurrency) CONCURRENCY="$2" ;;
        --service) SERVICE_FILTER="$2" ;;
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

for tool in kubectl jq curl go python3; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        gate_fail "$tool is not installed"
    fi
done
[ "$gate_failed" -ne 0 ] && gate_verdict "live"
gate_pass "kubectl, jq, curl, go, python3 present"

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

# The fleet must be SETTLED before load: a rollout in progress (Tilt rebuild,
# manual restart) means relays land while consumers are being replaced, and the
# short localnet claim/proof windows turn ordinary handover latency into
# missed windows. The rule: live validation runs if and only if no more
# changes are in flight and every pod is on the current ReplicaSet.
for dep in relayer miner; do
    if rollout_out="$(kubectl rollout status "deployment/${dep}" --timeout=5s 2>&1)"; then
        gate_pass "${dep}: rollout settled"
    else
        gate_fail "${dep}: rollout in progress -- run the gate only once the fleet is settled"
        gate_detail "$rollout_out" 3
    fi
done
[ "$gate_failed" -ne 0 ] && gate_verdict "live"

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

# STAKED suppliers only: the registry also lists not_staked leftovers, and a
# relay pinned to one of those is answered 503 ("supplier ... is not_staked").
# The count also feeds the thin-load warning and the settlement filter.
suppliers="$("$BIN" redis supplier --list 2>/dev/null | awk '/^pokt/ && $2 == "active" {print $1}')"
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
    local -a extra_args=()
    # One gRPC cell drives a NON-default method, so --grpc-method is exercised
    # against the real relayer instead of only in unit tests. It rides an
    # existing cell rather than firing extra probe relays: the settlement
    # assertion compares billed against what this ledger recorded as served, so
    # any relay sent outside the load phase would surface as "foreign traffic"
    # and fail the money assertion for the wrong reason.
    #
    # HealthCheck is one of the four methods tilt/backend-server/pb/demo.proto
    # serves, and it takes a request with no fields, so the default empty
    # --grpc-request-hex is correct. If --grpc-method regresses, this cell goes
    # red on verification or on served==billed -- which is the whole point:
    # before this, the custom-method path had no gate at any level.
    if [ "$mode" = "grpc" ] && [ "$service" = "${GRPC_CUSTOM_METHOD_SERVICE:-develop-grpc-optimistic}" ]; then
        extra_args=(--grpc-method "${GRPC_CUSTOM_METHOD:-/demo.DemoService/HealthCheck}")
    fi
    case "$mode" in
    jsonrpc | websocket | grpc)
        out="$("$BIN" relay "$mode" --localnet --service "$service" \
            --relayer-url "$relayer_url" \
            --load-test -n "$RELAYS_PER_TRANSPORT" --concurrency "$CONCURRENCY" \
            --all-suppliers "${extra_args[@]}" 2>&1)"
        rc=$?
        ;;
    stream | cometbft)
        # Sequential single requests; each successful invocation is one billed
        # relay. --all-suppliers does not apply (stream pins its supplier at
        # the handshake), and with --localnet the CLI pins EVERY invocation to
        # supplier1 when --supplier is absent -- successive requests do NOT
        # spread on their own (the old comment claiming they did was false).
        # All of this cell's relays go to ONE supplier, rotated per cell:
        # concentration keeps the per-supplier-session count at 3 (issue #25
        # reproduces at 1 relay/supplier-session), while rotating across cells
        # stops every cell from stacking on supplier1's session budget.
        out=""
        rc=0
        TRANSPORT_SENT=0
        local -a sup_arr
        # shellcheck disable=SC2206 # suppliers are newline-separated bech32 addresses, no globs
        sup_arr=($suppliers)
        local one sup
        sup="${sup_arr[$((STREAM_CELL_IDX % ${#sup_arr[@]}))]}"
        STREAM_CELL_IDX=$((STREAM_CELL_IDX + 1))
        for _ in $(seq 1 "$STREAM_CELL_RELAYS"); do
            if [ "$mode" = "stream" ]; then
                one="$("$BIN" relay stream --localnet --service "$service" \
                    --relayer-url "$relayer_url" --supplier "$sup" --batches 3 2>&1)" || rc=$?
            else
                one="$("$BIN" relay cometbft --localnet --service "$service" \
                    --relayer-url "$relayer_url" --supplier "$sup" 2>&1)" || rc=$?
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

# transport_expected_count MODE -- how many relays the cell ASKED for. The gate
# compares this against what came back: a relay that never got served is a loss
# too, just one that happens before the claim path this gate measures, and
# scoring the run against what succeeded would hide it by construction.
transport_expected_count() {
    case "$1" in
    jsonrpc | websocket | grpc) printf '%s' "$RELAYS_PER_TRANSPORT" ;;
    stream | cometbft) printf '%s' "$STREAM_CELL_RELAYS" ;;
    esac
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
matrix_overridden="${MATRIX+1}"
MATRIX="${MATRIX:-jsonrpc:develop-http jsonrpc:develop-http-eager websocket:develop-websocket websocket:develop-websocket-optimistic grpc:develop-grpc grpc:develop-grpc-optimistic stream:develop-stream stream:develop-stream-optimistic cometbft:develop-cometbft cometbft:develop-cometbft-optimistic}"
RELAYS_PER_TRANSPORT="${RELAYS_PER_TRANSPORT:-60}"
# stream/cometbft send one relay per invocation, three per cell.
STREAM_CELL_RELAYS=3
# Probes fired by the multi-backend distribution assert. They are REAL
# signed relays and are billed, so the number appears in two assertions.
BACKEND_PROBE_RELAYS=12
STREAM_CELL_IDX=0

# --service narrows the run to every cell of one service. Narrowing is
# deliberate, so it also skips the completeness cross-check below.
if [ -n "$SERVICE_FILTER" ]; then
    narrowed=""
    for pair in $MATRIX; do
        case "${pair#*:}" in
        "$SERVICE_FILTER") narrowed="${narrowed} ${pair}" ;;
        esac
    done
    if [ -z "$narrowed" ]; then
        printf 'no matrix cell matches --service %s\n' "$SERVICE_FILTER" >&2
        exit 2
    fi
    MATRIX="${narrowed# }"
    matrix_overridden=1
fi

# The default MATRIX must cover every staked develop-* service, and nothing
# ties the hand-written list to what the Tiltfile actually stakes -- a cell
# added to the mode matrix (or a renamed service) would simply never be loaded
# or asserted, and the gate would stay green while a whole transport x mode
# cell went unvalidated. Cross-check against the relayer's rendered config,
# which lists exactly the services the localnet serves. A deliberate MATRIX
# override skips this (narrowing is the override's purpose).
if [ -z "$matrix_overridden" ]; then
    staked_services="$(kubectl get configmap relayer-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null |
        python3 -c 'import sys,yaml; c=yaml.safe_load(sys.stdin) or {}; print("\n".join(sorted((c.get("services") or {}).keys())))' 2>/dev/null || true)"
    if [ -z "$staked_services" ]; then
        gate_fail "could not read the staked services from configmap relayer-config -- cannot prove the matrix is complete"
    else
        for svc in $staked_services; do
            case "$svc" in
            develop-*)
                case " $MATRIX " in
                *":${svc} "*) ;;
                *) gate_fail "staked service ${svc} is missing from the gate MATRIX -- its transport x mode cell is unvalidated" ;;
                esac
                ;;
            esac
        done
        for pair in $MATRIX; do
            svc="${pair#*:}"
            if ! printf '%s\n' "$staked_services" | grep -qx "$svc"; then
                gate_fail "MATRIX cell ${pair} names a service that is not staked -- stale matrix entry"
            fi
        done
        [ "$gate_failed" -eq 0 ] && gate_pass "MATRIX covers all $(printf '%s\n' "$staked_services" | grep -c 'develop-') staked develop-* services"
    fi
    [ "$gate_failed" -ne 0 ] && gate_verdict "live"
fi

# Issue #25: single-relay-per-supplier claims can vanish between the WAL and
# the claim (a supplier's entire 1-relay session goes unclaimed, with no drop
# counter and no failure state). Reproduced at 1 relay/supplier regardless of
# session timing; never observed at >=4 relays/supplier (300/300 billed). Until
# it is fixed, warn when the load is thin enough to trip it -- the exact
# served==billed assertion is then testing #25, not the change under test.
if [ "$RELAYS_PER_TRANSPORT" -lt $(( supplier_count * 2 )) ]; then
    gate_skip "RELAYS_PER_TRANSPORT=${RELAYS_PER_TRANSPORT} gives <2 relays per supplier (${supplier_count} suppliers): exact billing may trip issue #25"
fi
# stream/cometbft cells ignore RELAYS_PER_TRANSPORT: each sends 3 sequential
# relays pinned to ONE (per-cell rotated) supplier, so a session boundary can
# split them 2+1 and leave a 1-relay supplier-session -- the issue #25 regime
# the warning above cannot see for these transports.
case " $MATRIX " in
*" stream:"* | *" cometbft:"*)
    gate_skip "stream/cometbft cells send 3 relays on one supplier: a session-boundary split can trip issue #25 for that cell"
    ;;
esac

# Height at which THIS run's load begins: settlement events are later filtered
# to sessions ending at or after it, so claims from earlier traffic on a shared
# localnet (previous runs, bursts) cannot satisfy this run's expectations.
# A failed fetch must be a hard stop: an empty value would degrade the filter
# to ">= 0" and let ANY earlier session satisfy the exact-billing assertion.
# GRANULARITY: the filter is per SESSION. Traffic sent earlier within the
# session that is still open when this run starts (or one that closes right
# at the boundary) shares its session_end with this run's relays and is
# counted -- the billed>sent failure then reads "foreign traffic", which is
# accurate. Leave at least one full session (~200s on this localnet) between
# a previous load and a gate run.
load_start_height="$(curl -fsS --max-time 5 "${VALIDATOR_RPC}/status" 2>/dev/null |
    jq -r '.result.sync_info.latest_block_height // empty')"
if [ -z "$load_start_height" ]; then
    gate_fail "could not read the chain height before loading -- the per-run settlement filter would be void"
    gate_verdict "live"
fi

# Per-service expectation ledger: "mode service sent exact" per line. `exact`
# marks transports whose accounting model is pinned (one request signs one
# fresh relay, so served == billed). stream and cometbft are asserted as >=1
# proven and REPORTED, to pin their model empirically before demanding it.
matrix_ledger="${BIN_DIR}/matrix.tsv"
: >"$matrix_ledger"

# announced_drops SERVICE -- relays the miner explicitly refused for this
# service, summed over the reasons that mean "this relay can no longer reach a
# claim, and we said so": a tree already sealed for its claim, or a claim window
# already closed. Those are expected outcomes, not losses to hunt.
#
# It reads a COUNTER, which accumulates across runs, so every call is a delta
# against the snapshot taken before the load. Prints 0 when Prometheus cannot be
# reached, on purpose: the shortfall then stays unexplained and the assertion
# fails, because a scrape failure must never excuse a real loss.
announced_drop_reasons='session_sealed|claim_window_closed'

announced_drops_now() {
    curl -fsS --max-time 5 --get "${PROMETHEUS_URL}/api/v1/query" \
        --data-urlencode "query=sum by (service_id) (ha_miner_relays_rejected_total{reason=~\"${announced_drop_reasons}\"})" 2>/dev/null |
        jq -r '.data.result[]? | "\(.metric.service_id)\t\(.value[1])"' 2>/dev/null || true
}

drops_before="${BIN_DIR}/announced_drops_before.tsv"
announced_drops_now >"$drops_before" || : >"$drops_before"

# The relayer's mining-difficulty check FAILS OPEN: when the target hash cannot
# be resolved the relay is mined as applicable anyway, and
# relayer/relay_processor.go:177 says so in as many words -- "the counter is the
# only signal that difficulty is unresolvable". Nothing read that counter until
# this gate did, which meant a run with the difficulty filter completely broken
# was INDISTINGUISHABLE from a healthy one: every relay applicable, sent ==
# num_relays, green. The gate was asserting the filter's outcome while never
# checking the filter ran.
#
# Read as a delta over this run, like the drops above, because it is a counter.
difficulty_failures_now() {
    curl -fsS --max-time 5 --get "${PROMETHEUS_URL}/api/v1/query" \
        --data-urlencode "query=sum by (service_id) (ha_relayer_difficulty_query_failures_total)" 2>/dev/null |
        jq -r '.data.result[]? | "\(.metric.service_id)\t\(.value[1])"' 2>/dev/null || true
}

# Relays the filter DECLINED to mine. At base difficulty every relay is
# applicable, so this is 0 and its value is as a tripwire: a non-zero count here
# on a base-difficulty localnet means the effective target hash is not base, and
# then the per-service `sent == num_relays` assertion below is comparing across
# a filter and would fail for a reason that is not a loss.
skipped_difficulty_now() {
    curl -fsS --max-time 5 --get "${PROMETHEUS_URL}/api/v1/query" \
        --data-urlencode "query=sum by (service_id) (ha_relayer_relays_skipped_difficulty_total)" 2>/dev/null |
        jq -r '.data.result[]? | "\(.metric.service_id)\t\(.value[1])"' 2>/dev/null || true
}

difficulty_failures_before="${BIN_DIR}/difficulty_failures_before.tsv"
difficulty_failures_now >"$difficulty_failures_before" || : >"$difficulty_failures_before"
skipped_difficulty_before="${BIN_DIR}/skipped_difficulty_before.tsv"
skipped_difficulty_now >"$skipped_difficulty_before" || : >"$skipped_difficulty_before"

# TOTAL_DELTA SNAPSHOT_FILE READER -- sums one counter family across services.
counter_family_delta() {
    local before_file="$1" reader="$2" total=0 svc before after
    while IFS=$'\t' read -r svc after; do
        [ -n "${svc:-}" ] || continue
        before="$(awk -F'\t' -v s="$svc" '$1 == s {print int($2)}' "$before_file" | tail -1)"
        total=$(( total + $(gate_counter_delta "${before:-0}" "$(printf '%d' "${after%%.*}" 2>/dev/null || echo 0)") ))
    done < <("$reader")
    printf '%s' "$total"
}

announced_drops() {
    local svc="$1" before after
    before="$(awk -F'\t' -v s="$svc" '$1 == s {print int($2)}' "$drops_before" | tail -1)"
    after="$(announced_drops_now | awk -F'\t' -v s="$svc" '$1 == s {print int($2)}' | tail -1)"
    gate_counter_delta "${before:-0}" "${after:-0}"
}

loaded_services=""
for pair in $MATRIX; do
    mode="${pair%%:*}"
    service="${pair#*:}"

    gate_step "load: ${mode} -> ${service} via ${relayer_url}"

    if run_transport_load "$mode" "$service"; then
        succeeded="$(transport_success_count "$mode" "$TRANSPORT_OUT")"
        expected="$(transport_expected_count "$mode")"
        unserved="$(gate_served_shortfall "${expected:-0}" "${succeeded:-0}")"
        if [ "$unserved" -gt 0 ] && [ "${succeeded:-0}" -gt 0 ]; then
            # Recorded in the ledger anyway: the settlement assert below still
            # has something to say about the ones that DID get served, and the
            # run is already failing.
            gate_fail "${mode}: only ${succeeded} of ${expected} relays were served (${unserved} never made it) -- the loss is upstream of the claim path this gate measures"
            gate_detail "$(printf '%s\n' "$TRANSPORT_OUT" | tail -15)"
        fi
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
# Multi-backend distribution (absorbed from the retired test-round-robin.sh,
# which measured this through PATH and could not tell a relayer 503 from a
# served relay). The demo backend stamps backend_id into eth_blockNumber
# responses; when the rendered config gives develop-http more than one
# jsonrpc backend, a handful of signed single relays must land on more than
# one of them, or the pool is not distributing.
case " $MATRIX " in
*" jsonrpc:develop-http "*)
    # Capture the configmap FIRST: piping kubectl straight into python under
    # pipefail makes a kubectl failure emit "0" twice (python prints 0 for
    # empty stdin AND the || fallback fires on the pipeline status), and the
    # doubled value blows up the -gt test, skipping this block silently.
    rendered_relayer_config="$(kubectl get configmap relayer-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null || true)"
    if [ -z "$rendered_relayer_config" ]; then
        gate_skip "could not read relayer-config for the distribution assert"
        backend_count=0
        lb_mode=""
    else
        backend_count="$(printf '%s' "$rendered_relayer_config" |
            python3 -c 'import sys,yaml; c=yaml.safe_load(sys.stdin) or {}; b=(((c.get("services") or {}).get("develop-http") or {}).get("backends") or {}).get("jsonrpc") or {}; print(len(b.get("urls") or []))' 2>/dev/null || echo 0)"
        lb_mode="$(printf '%s' "$rendered_relayer_config" |
            python3 -c 'import sys,yaml; c=yaml.safe_load(sys.stdin) or {}; b=(((c.get("services") or {}).get("develop-http") or {}).get("backends") or {}).get("jsonrpc") or {}; print(b.get("load_balancing") or "round_robin")' 2>/dev/null || echo "")"
    fi
    # Only round_robin promises to SPREAD load; first_healthy with multiple
    # urls is legitimate failover-only config where all relays landing on one
    # backend is the correct behavior, not a distribution bug.
    if [ "${backend_count:-0}" -gt 1 ] && [ "$lb_mode" != "round_robin" ]; then
        gate_skip "develop-http has ${backend_count} backends but load_balancing=${lb_mode:-unknown}; distribution assert only applies to round_robin"
    fi
    if [ "${backend_count:-0}" -gt 1 ] && [ "$lb_mode" = "round_robin" ]; then
        gate_step "assert: multi-backend distribution on develop-http (${backend_count} backends)"
        seen_backends=""
        rr_served=0
        for _ in $(seq 1 "$BACKEND_PROBE_RELAYS"); do
            rr_out="$("$BIN" relay jsonrpc --localnet --service develop-http \
                --relayer-url "$relayer_url" 2>/dev/null)" && rr_served=$((rr_served + 1))
            bid="$(printf '%s' "$rr_out" | grep -o '"backend_id":"[^"]*"' | head -1 | cut -d'"' -f4)"
            [ -n "$bid" ] && case " $seen_backends " in
            *" $bid "*) ;;
            *) seen_backends="${seen_backends} ${bid}" ;;
            esac
        done
        distinct="$(printf '%s\n' $seen_backends | grep -c . || true)"
        probe_unserved="$(gate_served_shortfall "$BACKEND_PROBE_RELAYS" "${rr_served:-0}")"
        if [ "$probe_unserved" -gt 0 ]; then
            gate_fail "backend probe: only ${rr_served} of ${BACKEND_PROBE_RELAYS} relays were served"
        fi
        if [ "${distinct:-0}" -ge 2 ]; then
            gate_pass "load spread across ${distinct} backends:${seen_backends}"
        else
            gate_fail "${BACKEND_PROBE_RELAYS} relays all landed on one backend (${seen_backends:-none}) with ${backend_count} configured -- pool not distributing"
        fi
        # These probes are REAL signed relays: they mine and bill in the same
        # sessions the exact served==billed assertion counts. Add them to the
        # ledger's develop-http row or the cell fails with billed>sent by
        # exactly the probe count.
        if [ "$rr_served" -gt 0 ]; then
            awk -F'\t' -v OFS='\t' -v add="$rr_served" \
                '$2 == "develop-http" { $3 += add } { print }' \
                "$matrix_ledger" >"${matrix_ledger}.tmp" && mv "${matrix_ledger}.tmp" "$matrix_ledger"
        fi
    fi
    ;;
esac

# ---------------------------------------------------------------------------
gate_step "assert: the mining-difficulty filter actually ran"

# This runs BEFORE the settlement wait on purpose: if the filter was down, the
# per-service assertions below are comparing numbers the filter never touched,
# and there is no reason to spend 25 minutes discovering that.
#
# "Zero failures" and "could not measure" MUST NOT produce the same signal.
# Every query here returns empty when Prometheus is unreachable, and empty sums
# to 0 -- which would read as a clean run. So liveness is asserted first, with a
# counter that MUST have series after the load, and the outcome when it does not
# is gate_nothing_measured, never a pass.
prom_series_count() {
    curl -fsS --max-time 5 --get "${PROMETHEUS_URL}/api/v1/query" \
        --data-urlencode "query=count(ha_relayer_relays_published_total)" 2>/dev/null |
        jq -r '.data.result[0].value[1] // "0"' 2>/dev/null || printf '0'
}
published_series="$(prom_series_count)"

if [ "${published_series%%.*}" -lt 1 ] 2>/dev/null || [ -z "$published_series" ]; then
    gate_nothing_measured "difficulty filter: Prometheus returned no relays_published series after a load that served relays -- the filter's counters cannot be read, so this run proves nothing about it"
else
    difficulty_failures="$(counter_family_delta "$difficulty_failures_before" difficulty_failures_now)"
    skipped_difficulty="$(counter_family_delta "$skipped_difficulty_before" skipped_difficulty_now)"

    if [ "${difficulty_failures:-0}" -gt 0 ]; then
        gate_fail "difficulty filter: ${difficulty_failures} relay(s) could not resolve a target hash and were mined ANYWAY (it fails open, relay_processor.go:181) -- every assertion below about served==billed passed through a filter that was not working"
    else
        gate_pass "difficulty filter: resolved a target hash for every relay (0 query failures)"
    fi

    if [ "${skipped_difficulty:-0}" -gt 0 ]; then
        gate_fail "difficulty filter: ${skipped_difficulty} relay(s) were filtered out as non-applicable, so this chain is NOT at base difficulty -- the exact served==billed assertions below do not hold across a filter and this gate does not yet cover that regime"
    else
        gate_pass "difficulty filter: base difficulty, 0 relays filtered -- served==billed is the right assertion for this run"
    fi
fi

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

# The exercise unit for this gate is relays BILLED on-chain, which is the only
# number that cannot be produced by a run that did nothing: a live gate that
# settled zero relays has not exercised the money path, whatever its per-service
# assertions say about the services it found. all.sh turns a zero into NOT RUN.
billed_total=0

while IFS=$'\t' read -r mode svc sent exact; do
    [ -z "$svc" ] && continue
    read -r proven_n relays_n <<<"$(billed_relays "$svc")"
    billed_total=$((billed_total + ${relays_n:-0}))

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
            # A shortfall is only acceptable to the extent the miner ANNOUNCED
            # it. A relay that arrives after its tree was sealed, or after its
            # claim window closed, cannot be paid and there is nothing to
            # recover -- but it must have been counted. Anything the counters do
            # not account for is the silent loss this gate exists to catch, and
            # still fails.
            dropped="$(announced_drops "$svc")"
            unexplained="$(gate_unexplained_shortfall "$sent" "${relays_n:-0}" "${dropped:-0}")"
            if [ "$unexplained" -eq 0 ] && [ "${dropped:-0}" -gt 0 ]; then
                gate_pass "${svc} (${mode}): ${relays_n}/${sent} relays billed across ${proven_n} proven claim(s)"
                printf '         + %s dropped, announced as %s (accounted)\n' \
                    "$dropped" "$(printf '%s' "$announced_drop_reasons" | tr '|' '/')"
            else
                gate_fail "${svc} (${mode}): served ${sent}, billed ${relays_n:-0}, announced drops ${dropped:-0} -- ${unexplained} relay(s) LOST with no counter"
                printf '         check the WAL (redis streams) and submissions for this service\n'
            fi
        fi
    else
        if [ "${proven_n:-0}" -gt 0 ] && [ "${relays_n:-0}" -gt 0 ]; then
            gate_pass "${svc} (${mode}): ${proven_n} claim(s) PROVEN, sent=${sent} billed=${relays_n} (model unpinned: reported, not asserted)"
        else
            gate_fail "${svc} (${mode}): served relays but NONE were billed (proven=${proven_n:-0})"
        fi
    fi
done <"$matrix_ledger"

gate_exercised coverage billed_relays "$billed_total"

# Terminal-event assertions are scoped to THIS run's sessions, the same
# session_end_block_height >= load_start_height filter the billed counter
# uses (every one of these event types carries the field -- verified against
# poktroll x/tokenomics event.pb.go). Without it, a claim from an EARLIER
# run expiring while this gate polls fails THIS run: proof windows close up
# to ~5.5 min after their session ends, well inside our scan window, and the
# supplier filter alone matches all localnet traffic. A missing attribute
# counts as in-window: for a gate, a false red beats a silent pass.
count_terminal_events() {
    jq -rs --arg type "$1" --argjson minend "${load_start_height:-0}" '
        [.[] | select(.type == $type)
             | select((.attrs.session_end_block_height == null)
                 or ((.attrs.session_end_block_height | tostring | gsub("[^0-9]";"") | if . == "" then "0" else . end | tonumber) >= $minend))]
        | length' "$events_file" 2>/dev/null || echo 0
}

expired="$(count_terminal_events 'pocket.tokenomics.EventClaimExpired')"
slashed="$(count_terminal_events 'pocket.tokenomics.EventSupplierSlashed')"
discarded="$(count_terminal_events 'pocket.tokenomics.EventClaimDiscarded')"
settled_invalid="$(jq -rs --argjson minend "${load_start_height:-0}" '
    [.[] | select(.type == "pocket.tokenomics.EventClaimSettled")
         | select(.attrs.claim_proof_status_int // "" | tostring | test("^\"?2\"?$"))
         | select((.attrs.session_end_block_height == null)
             or ((.attrs.session_end_block_height | tostring | gsub("[^0-9]";"") | if . == "" then "0" else . end | tonumber) >= $minend))]
    | length' "$events_file" 2>/dev/null || echo 0)"

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
# with the chain, and a disagreement is itself the finding. One --json listing
# per supplier, states counted with jq -- the previous form grepped for lines
# starting with "session", which the CLI's table output never produces (rows
# start with the bare hex session ID), so the check could never fire.
fail_states_file="${BIN_DIR}/miner_session_states.txt"
: >"$fail_states_file"
for supplier in $suppliers; do
    "$BIN" redis sessions --supplier "$supplier" --json 2>/dev/null |
        jq -r '(if type == "array" then . else [] end)[] | .state // empty' 2>/dev/null
done >>"$fail_states_file"
for state in claim_missing claim_tx_error proof_tx_error proof_window_closed claim_window_closed; do
    n="$(grep -cx "$state" "$fail_states_file" 2>/dev/null || true)"
    [ "${n:-0}" -gt 0 ] && gate_fail "miner reports ${n} session(s) in failure state '${state}'"
done

if [ "$resolved" -eq 0 ]; then
    gate_fail "timed out after ${SETTLE_TIMEOUT_MIN} min with services still unpaid"
fi

# --- Settlement breakdown -----------------------------------------------------
#
# Reporting only. Nothing here fails the gate, and that is deliberate: the two
# loss channels below are EXPECTED to read zero on a healthy localnet, so a
# non-zero would be the finding while a zero proves nothing. Printing them keeps
# "did not occur" and "was never looked at" from producing the same signal.
#
# The numbers are already on disk. scan_settlement_events captures every
# attribute of every pocket.tokenomics.Event, so the settlement breakdown has
# been collected on every run since that function existed -- only num_relays was
# ever read out of it.
#
# THREE relay counts exist and they are not interchangeable:
#
#   sent (the CLI's count)   relays served, BEFORE the relayer's difficulty filter
#   num_relays               leaves in the submitted tree, i.e. only the relays
#                            whose hash matched the service's mining difficulty
#   num_estimated_relays     num_relays x the difficulty multiplier, the chain's
#                            estimate of the work actually done
#
# The gate's billing assertion compares sent against num_relays, which spans the
# difficulty filter and is therefore only sound at BASE difficulty. Localnet runs
# there (every service reports an unset target hash, which x/service resolves to
# BaseRelayDifficultyHashBz, multiplier 1), so the two coincide and the assertion
# holds. On a network with real difficulty it would not. Both are printed so the
# day they diverge is visible rather than inferred.

gate_step "settlement breakdown (fails only if jq itself broke; overservicing/deflation values are reporting-only)"
sb_line="$(gate_settlement_breakdown "$events_file" "${load_start_height:-0}")"
sb_rc=$?
IFS=$'\t' read -r sb_claims sb_relays sb_estimated sb_claimed sb_settled sb_minted \
    sb_overloss sb_deflation sb_over_events sb_spend_limit <<<"$sb_line"

gate_detail "claims settled           ${sb_claims:-0}
num_relays (tree leaves) ${sb_relays:-0}
num_estimated_relays     ${sb_estimated:-0}
claimed_upokt            ${sb_claimed:-0}
settled_upokt            ${sb_settled:-0}
minted_upokt             ${sb_minted:-0}" 12

# gate_settlement_breakdown returns non-zero when jq itself failed to parse
# the chain's events (schema change, malformed JSON) -- distinct from an
# empty events file, which is not an error and is handled inside that
# function. Failing loudly here, rather than reading the zeros it still
# prints as "nothing happened", is the entire point of MEDIUM-3 (review
# 2026-08-20): a parse failure and a genuinely quiet run must never look the
# same on a money check.
if [ "$sb_rc" -ne 0 ]; then
    gate_fail "settlement breakdown: jq failed to parse ${events_file} (see error above) -- overservicing/deflation below are UNKNOWN, not zero"
else
    # Overservicing: the application's stake could not cover the claim, so the
    # chain paid less than was claimed. It is money that did not arrive, and it
    # does NOT show up in any relay count -- claimed and settled relay counts
    # both stay put while the uPOKT shrinks, so a check that only counts relays
    # cannot see it.
    if [ "${sb_overloss:-0}" -gt 0 ] || [ "${sb_over_events:-0}" -gt 0 ]; then
        gate_pass "overservicing OCCURRED: ${sb_overloss} uPOKT across ${sb_over_events} event(s), ${sb_spend_limit} from a per-session spend limit"
    else
        gate_pass "overservicing did not occur this run (0 events, 0 uPOKT) -- not evidence that it cannot"
    fi

    # Deflation is mint_ratio < 1, a governance parameter rather than a defect
    # here. Reported separately so it is never mistaken for the line above.
    if [ "${sb_deflation:-0}" -gt 0 ]; then
        gate_pass "deflation (mint_ratio < 1): ${sb_deflation} uPOKT -- a governance parameter, not a fault"
    else
        gate_pass "no deflation this run (mint_ratio = 1)"
    fi
fi

gate_verdict "live"
