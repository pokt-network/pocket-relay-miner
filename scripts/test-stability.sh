#!/bin/bash
# test-stability.sh - Run test suite 100 times to detect flaky tests
# Usage: ./scripts/test-stability.sh [package]
# Example: ./scripts/test-stability.sh miner
#
# Per CLAUDE.md Rule #1: No flaky tests, no race conditions, no mock/fake tests
# Any test that fails once in 100 runs must be fixed or skipped with TODO comment.
#
# This script is used by:
# 1. Nightly CI stability checks
# 2. Manual validation before claiming tests are stable
#
# Exit codes:
#   0 - All 100 runs passed
#   1 - At least one run failed (flaky test detected)

set -euo pipefail

RUNS=${RUNS:-100}
PACKAGE=${1:-"./..."}

echo "=============================================="
echo "Flaky Test Detection - $RUNS runs"
echo "=============================================="
echo "Package: $PACKAGE"
echo "Race detection: enabled"
echo "Shuffle: enabled"
echo ""
echo "Per CLAUDE.md Rule #1: Any failure = flaky = must fix"
echo ""

FAILURES=0
for i in $(seq 1 $RUNS); do
    printf "Run %3d/$RUNS... " "$i"

    # Run with -race and -shuffle to maximize variance
    # -count=1 disables test caching
    if go test -race -shuffle=on -count=1 -tags test -p 4 -parallel 4 "$PACKAGE" &>/dev/null; then
        echo "PASS"
    else
        echo "FAIL"
        ((FAILURES++))

        echo ""
        echo "=============================================="
        echo "FLAKY TEST DETECTED on run $i"
        echo "=============================================="
        echo "Rerunning with verbose output to capture failure:"
        echo ""

        # Rerun to capture output
        go test -race -shuffle=on -count=1 -tags test -v "$PACKAGE" || true

        echo ""
        echo "=============================================="
        echo "Action required:"
        echo "1. Identify the failing test from output above"
        echo "2. Either fix the flakiness or skip with:"
        echo "   t.Skip(\"TODO(phase3): fix flaky test - <reason>\")"
        echo "=============================================="
        exit 1
    fi
done

echo ""
echo "=============================================="
if [ $FAILURES -eq 0 ]; then
    echo "SUCCESS: All $RUNS runs passed"
    echo "Test suite is stable (no flaky tests detected)"
else
    echo "FAILED: $FAILURES/$RUNS runs failed"
    echo "Flaky tests detected - see output above"
    exit 1
fi
echo "=============================================="
