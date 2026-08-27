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

gate_step "go test -coverprofile $pkg"

if out="$(go test -tags test "${parallelism[@]}" -coverprofile=coverage.out "$pkg" 2>&1)"; then
    # Computed from coverage.out, whose format IS a contract ("mode:" then
    # `file:l.c,l.c numstmt count`), never from `go tool cover -func`'s human
    # table. A counter that parses a foreign tool's prose goes to zero the day
    # that prose changes, which is how an anti-vacuity ledger becomes vacuous --
    # budgetkit measured exactly that on 2026-08-26. Verified the same day: this
    # computation and `go tool cover` both report 45.5% on this tree.
    read -r covered_stmts total_pct <<<"$(awk 'NR > 1 {
        tot += $2
        if ($3 > 0) cov += $2
    } END {
        if (tot > 0) printf "%d %.1f%%\n", cov, 100 * cov / tot; else print "0 "
    }' coverage.out 2>/dev/null)"
    total="$total_pct"
    # An EMPTY total is the vacuous case, and it used to be invisible: the
    # number was optional in the message (`${total:+...}`), so a profile that
    # measured nothing printed "coverage run clean" and the gate passed.
    # Two vacuous shapes, and the SECOND is the one this actually produces --
    # predicted empty, measured "0.0%" (2026-08-26, PKG pointed at a package
    # with no test files). A genuine 0.0% over a tree that has tests is itself a
    # reason to stop, so both are NOT RUN rather than PASS.
    case "${total:-}" in
    "")
        gate_nothing_measured "the coverage gate measured nothing: the profile has no total, so nothing was covered"
        ;;
    0.0% | 0%)
        gate_nothing_measured "the coverage gate measured nothing: total is $total for $pkg, so no statement was exercised"
        ;;
    *)
        gate_pass "coverage run clean -- total $total"
        # Functions the profile actually measured, not the percentage: a total
        # can look plausible over a handful of statements.
        # Covered STATEMENTS, not functions listed by a formatter: it is the
        # thing the percentage is a ratio of, and it comes from the same file.
        gate_exercised coverage covered_statements "${covered_stmts:-0}"
        ;;
    esac

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
