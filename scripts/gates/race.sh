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

# These suites include tests that assert REAL Redis semantics (blocking reads,
# PEL ageing) which miniredis does not reproduce. Bring one up; it is a single
# container reused across the whole run, on its own port, never the localnet's.
# Captured first and evaluated second, deliberately: the exit status of
# `eval "$(cmd)"` is the status of the string it evaluates, not of cmd, so a
# failing redis.sh would be swallowed and the run would collapse into a wall of
# per-test fatals instead of one diagnostic.
if ! redis_env="$(./scripts/gates/redis.sh up)"; then
    gate_fail "could not provide a real Redis for the tests"
    gate_verdict "$(basename "${BASH_SOURCE[0]}" .sh)"
fi
eval "$redis_env"
export REDIS_TEST_URL

pkg="$(gate_pkg_target)"

read -r -a parallelism <<<"$(gate_parallelism)"

gate_step "go test -race $pkg"

json_out="$(mktemp)"
if go test -json -race -count=1 -tags test ${parallelism[@]+"${parallelism[@]}"} "$pkg" >"$json_out" 2>&1; then
    ok_count="$(gate_pkg_passes "$json_out")"
    if [ "$ok_count" -eq 0 ]; then
        gate_nothing_measured "no package ran under the race detector for $pkg, so no schedule was observed"
    else
        gate_pass "$ok_count package(s) passed under the race detector"
        gate_exercised coverage race_packages "$ok_count"
    fi
else
    gate_fail "race detector or tests failed:"
    # `tail`, not the head the fallback used to print: when the grep matches
    # nothing the interesting part is the END of the run, and the head is a wall
    # of passes that says nothing about the failure (measured 2026-08-27).
    gate_detail "$(gate_json_output "$json_out" | grep -E 'DATA RACE|^(---|FAIL|panic)' || gate_json_output "$json_out" | tail -40)" 40
    gate_keep_evidence "$json_out" race
fi
rm -f "$json_out"

gate_verdict "race"
