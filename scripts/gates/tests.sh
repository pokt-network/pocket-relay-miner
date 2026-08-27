#!/usr/bin/env bash
#
# Gate: the test suite. Minutes, no cluster.
#
#   go test -tags test ./...
#
# Usage:
#   scripts/gates/tests.sh            # whole tree
#   PKG=miner scripts/gates/tests.sh  # one package
#   VERBOSE=1 scripts/gates/tests.sh  # -v
#
# The `test` build tag is not optional: test-only helpers are behind it, and
# without the tag the suite compiles into a different, smaller program than the
# one this repository means by "the tests".
#
# The cache, miner and relayer packages run sequentially when targeted on their
# own: their tests mutate process-wide state in place, so concurrent tests would
# read each other's writes. The flags are not the guard -- they do not apply to
# the whole-tree run -- TestNoTestParallelWhereStateIsShared in
# internal/conventions is. gate_parallelism in lib.sh holds the full reasoning.
# This mirrors `make test`; keep the two in step.

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

verbose=()
[ -n "${VERBOSE:-}" ] && verbose=(-v)

pkg="$(gate_pkg_target)"

read -r -a parallelism <<<"$(gate_parallelism)"

case "${PKG:-}" in
cache | miner | relayer) gate_step "go test $pkg (sequential -- process-wide test state)" ;;
*) gate_step "go test $pkg" ;;
esac

# ${arr[@]+"${arr[@]}"} instead of "${arr[@]}": under `set -u`, expanding an
# EMPTY array aborts with "unbound variable" on bash < 4.4 -- and macOS ships
# 3.2, so the plain form kills the default `make test` path there with an
# empty "FAIL tests failed:" (the real error goes to uncaptured stderr).
json_out="$(mktemp)"
if go test -json ${verbose[@]+"${verbose[@]}"} -tags test ${parallelism[@]+"${parallelism[@]}"} "$pkg" >"$json_out" 2>&1; then
    # Counted from the -json contract, never from the `ok ` prose. See gate_pkg_passes.
    ok_count="$(gate_pkg_passes "$json_out")"
    no_test="$(gate_json_output "$json_out" | grep -c 'no test files' || true)"
    if [ "$ok_count" -eq 0 ]; then
        gate_nothing_measured "no package passed for $pkg: $no_test had no test files, so nothing was exercised"
    else
        gate_pass "$ok_count package(s) passed, $no_test with no test files"
        gate_exercised coverage packages "$ok_count"
    fi
else
    gate_fail "tests failed:"
    gate_detail "$(gate_json_output "$json_out" | grep -E '^(---|FAIL|panic|\s+.*_test\.go:)' || gate_json_output "$json_out")" 40
fi
rm -f "$json_out"

gate_verdict "tests"
