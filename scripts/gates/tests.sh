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
# The cache package runs sequentially. Its ~143 tests share one miniredis
# instance, so running them in parallel is a race against a fixture rather than
# a test of the code. This mirrors `make test`; keep the two in step.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

verbose=()
[ -n "${VERBOSE:-}" ] && verbose=(-v)

pkg="$(gate_pkg_target)"

read -r -a parallelism <<<"$(gate_parallelism)"

case "${PKG:-}" in
cache | miner) gate_step "go test $pkg (sequential -- shared miniredis fixture)" ;;
*) gate_step "go test $pkg" ;;
esac

# ${arr[@]+"${arr[@]}"} instead of "${arr[@]}": under `set -u`, expanding an
# EMPTY array aborts with "unbound variable" on bash < 4.4 -- and macOS ships
# 3.2, so the plain form kills the default `make test` path there with an
# empty "FAIL tests failed:" (the real error goes to uncaptured stderr).
if out="$(go test ${verbose[@]+"${verbose[@]}"} -tags test ${parallelism[@]+"${parallelism[@]}"} "$pkg" 2>&1)"; then
    # Report what actually ran, so a suite that silently compiled to nothing is
    # visible rather than reading as success.
    ok_count="$(printf '%s\n' "$out" | grep -c '^ok ' || true)"
    no_test="$(printf '%s\n' "$out" | grep -c 'no test files' || true)"
    gate_pass "$ok_count package(s) passed, $no_test with no test files"
else
    gate_fail "tests failed:"
    gate_detail "$(printf '%s\n' "$out" | grep -E '^(---|FAIL|panic|\s+.*_test\.go:)' || printf '%s' "$out")" 40
fi

gate_verdict "tests"
