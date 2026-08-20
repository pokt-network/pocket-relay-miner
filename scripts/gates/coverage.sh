#!/usr/bin/env bash
#
# Gate: the coverage run. Minutes, no cluster.
#
#   go test -tags test -coverprofile=coverage.out ./...
#
# Usage:
#   scripts/gates/coverage.sh             # whole tree
#   PKG=miner scripts/gates/coverage.sh   # one package
#   COVERAGE_HTML=1 scripts/gates/coverage.sh   # also write coverage.html
#
# This is what CI actually rejects on (.github/workflows/ci.yml runs both
# `make test` and `make test-coverage`), and it is NOT the same run as
# scripts/gates/tests.sh: instrumenting for coverage widens timing, which
# surfaces flakes a plain `go test` never shows. Passing tests.sh and failing
# here is a normal outcome, not a contradiction -- run this before any push.
#
# coverage.out is a build artifact and is written to the working tree. That is
# the one deliberate exception to "a gate has no side effects", because the
# profile is the point of the run; it is gitignored.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

pkg="$(gate_pkg_target)"

read -r -a parallelism <<<"$(gate_parallelism)"

gate_step "go test -coverprofile $pkg"

if out="$(go test -tags test "${parallelism[@]}" -coverprofile=coverage.out "$pkg" 2>&1)"; then
    total="$(go tool cover -func=coverage.out 2>/dev/null | awk '/^total:/ {print $3}')"
    gate_pass "coverage run clean${total:+ -- total ${total}}"

    if [ -n "${COVERAGE_HTML:-}" ]; then
        if go tool cover -html=coverage.out -o coverage.html 2>/dev/null; then
            gate_pass "coverage.html written"
        else
            gate_fail "could not render coverage.html"
        fi
    fi
else
    gate_fail "tests failed under coverage instrumentation:"
    gate_detail "$(printf '%s\n' "$out" | grep -E '^(---|FAIL|panic)' || printf '%s' "$out")" 40
    printf '         coverage timing differs from a plain run -- this can be a\n'
    printf '         real flake that %smake test%s hides, not a coverage bug\n' \
        "$GATE_BOLD" "$GATE_RESET"
fi

gate_verdict "coverage"
