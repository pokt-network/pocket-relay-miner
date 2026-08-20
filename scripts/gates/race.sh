#!/usr/bin/env bash
#
# Gate: the race detector. Minutes, no cluster.
#
#   go test -tags test -race -count=1 ./...
#
# Usage:
#   scripts/gates/race.sh             # whole tree
#   PKG=miner scripts/gates/race.sh   # one package
#
# CLAUDE.md calls this Rule #1 and says it cannot be broken. Until this gate
# existed the only target that passed -race was `test_miner`, covering
# ./miner/... alone, and CI invoked neither -- the rule was declared and never
# executed.
#
# -count=1 disables the test result cache. A cached PASS from a run without
# -race would satisfy the command while proving nothing about races.
#
# A race detector finding is never "flaky". The detector reports a race it
# observed; not observing one on the next run means the schedule differed, not
# that the race is gone. Re-running until green is how a race reaches
# production.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

pkg="$(gate_pkg_target)"

read -r -a parallelism <<<"$(gate_parallelism)"

gate_step "go test -race $pkg"

if out="$(go test -tags test -race -count=1 "${parallelism[@]}" "$pkg" 2>&1)"; then
    ok_count="$(printf '%s\n' "$out" | grep -c '^ok ' || true)"
    gate_pass "$ok_count package(s) passed under the race detector"
else
    # Separate an actual data race from an ordinary test failure: they need
    # different responses, and conflating them is how a race gets triaged as a
    # flake.
    if printf '%s\n' "$out" | grep -q 'WARNING: DATA RACE'; then
        gate_fail "DATA RACE detected:"
        gate_detail "$(printf '%s\n' "$out" | grep -A 20 'WARNING: DATA RACE')" 40
        printf '         a race is not a flake -- do not re-run for a green\n'
    else
        gate_fail "tests failed under -race (no data race reported):"
        gate_detail "$(printf '%s\n' "$out" | grep -E '^(---|FAIL|panic)' || printf '%s' "$out")" 40
    fi
fi

gate_verdict "race"
