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

if [ "$failures" -ne 0 ]; then
    printf 'lib_test: %s failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'lib_test: all cases pass\n'
