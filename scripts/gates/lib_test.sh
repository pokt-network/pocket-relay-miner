#!/usr/bin/env bash
#
# Self-test for the pure helpers in lib.sh. Runs inside the static gate, where
# it costs milliseconds. It exists because gate_unexplained_shortfall decides
# whether the live gate excuses a missing relay: an assertion that has never
# been red is decoration, and this one guards money.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

failures=0

expect() {
    local want="$1" got="$2" what="$3"
    if [ "$want" != "$got" ]; then
        printf '  FAIL %s: want %s, got %s\n' "$what" "$want" "$got" >&2
        failures=$((failures + 1))
    fi
}

# sent billed dropped -> unexplained
expect 0 "$(gate_unexplained_shortfall 72 72 0)"  "nothing missing"
expect 0 "$(gate_unexplained_shortfall 72 66 6)"  "every missing relay announced"
expect 3 "$(gate_unexplained_shortfall 72 66 3)"  "half announced, half silent"
expect 6 "$(gate_unexplained_shortfall 72 66 0)"  "NOTHING announced -- the loss this gate exists to catch"
expect 0 "$(gate_unexplained_shortfall 72 66 9)"  "more announced than missing is not evidence of a loss"
expect 0 "$(gate_unexplained_shortfall 0 0 0)"    "empty run"
expect 5 "$(gate_unexplained_shortfall 5 0 0)"    "everything lost, nothing said"

# expected served -> missing
expect 0  "$(gate_served_shortfall 60 60)" "everything asked for was served"
expect 20 "$(gate_served_shortfall 60 40)" "a third never made it -- scoring against 40 would hide it"
expect 60 "$(gate_served_shortfall 60 0)"  "nothing served"
expect 0  "$(gate_served_shortfall 60 61)" "more than asked is not a shortfall"
expect 0  "$(gate_served_shortfall 0 0)"   "empty cell"

# gate_settlement_breakdown: parses money out of JSON-encoded event attributes,
# where a silent parse failure returns a clean-looking zero. A zero from this
# helper is read as "overservicing did not occur"; if it can also mean "the parse
# broke", the report is worse than nothing.
sb_fixture="$(mktemp)"
trap 'rm -f "$sb_fixture"' EXIT
cat >"$sb_fixture" <<'FIXTURE'
{"height":"100","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"100\"","num_relays":"\"4\"","num_estimated_relays":"\"4\"","claimed_upokt":"\"1000upokt\"","settled_upokt":"\"1000upokt\"","minted_upokt":"\"1000upokt\"","overservicing_loss_upokt":"\"0\"","deflation_loss_upokt":"\"0\""}}
{"height":"100","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"100\"","num_relays":"\"6\"","num_estimated_relays":"\"12\"","claimed_upokt":"\"3000upokt\"","settled_upokt":"\"2000upokt\"","minted_upokt":"\"1800upokt\"","overservicing_loss_upokt":"\"1000\"","deflation_loss_upokt":"\"200\""}}
{"height":"100","type":"pocket.tokenomics.EventApplicationOverserviced","attrs":{"spend_limit_exceeded":"true"}}
{"height":"50","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"50\"","num_relays":"\"99\"","num_estimated_relays":"\"99\"","claimed_upokt":"\"9999upokt\"","settled_upokt":"\"9999upokt\"","minted_upokt":"\"9999upokt\"","overservicing_loss_upokt":"\"7\"","deflation_loss_upokt":"\"7\""}}
FIXTURE

# The height-50 claim is BEFORE the window and must be excluded; leaving it in
# would inflate every number and, worse, invent 7 uPOKT of overservicing from a
# previous run.
expect "$(printf '2\t10\t16\t4000\t3000\t2800\t1000\t200\t1\t1')" \
    "$(gate_settlement_breakdown "$sb_fixture" 100)" \
    "settlement breakdown, window at 100"

# estimated (16) differs from relays (10) on purpose: summing them together, or
# reading one for the other, is the difficulty-multiplier confusion this helper
# exists to keep visible.
expect "$(printf '3\t109\t115\t13999\t12999\t12799\t1007\t207\t1\t1')" \
    "$(gate_settlement_breakdown "$sb_fixture" 0)" \
    "no window: the older claim is included"

expect "$(printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0')" \
    "$(gate_settlement_breakdown /nonexistent/events.jsonl 0)" \
    "a missing events file yields zeros, not an error"

if [ "$failures" -ne 0 ]; then
    printf 'lib_test: %s failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'lib_test: all cases pass\n'
