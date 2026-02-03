#!/bin/bash
# test-coverage.sh - Measure test coverage for critical packages
# Usage: ./scripts/test-coverage.sh [--ci] [--html]
#
# Options:
#   --ci   Output in CI-friendly format (no colors, machine-parseable)
#   --html Generate HTML coverage report
#
# Exit codes:
#   0 - Success (coverage measured)
#   1 - Script error (not a coverage failure)
#
# Note: This script warns but does not fail on low coverage per Phase 2 decision.

set -e

CI_MODE=false
HTML_MODE=false
THRESHOLD=80
COVERDIR="coverage"

# Parse arguments
for arg in "$@"; do
  case $arg in
    --ci) CI_MODE=true ;;
    --html) HTML_MODE=true ;;
  esac
done

# Create coverage directory
mkdir -p "$COVERDIR"

# Packages to measure (critical paths)
PACKAGES=(
  "github.com/pokt-network/pocket-relay-miner/miner"
  "github.com/pokt-network/pocket-relay-miner/relayer"
  "github.com/pokt-network/pocket-relay-miner/cache"
)

# Collect coverage for each package
echo "Collecting coverage for critical packages..."

for pkg in "${PACKAGES[@]}"; do
  pkg_name=$(basename "$pkg")
  echo "  Testing $pkg_name..."
  go test -tags test -race -coverprofile="$COVERDIR/${pkg_name}.cover" -covermode=atomic "$pkg/..." 2>/dev/null || true
done

# Merge coverage files
echo "" > "$COVERDIR/combined.cover"
echo "mode: atomic" > "$COVERDIR/combined.cover"
for f in "$COVERDIR"/*.cover; do
  if [ -f "$f" ] && [ "$f" != "$COVERDIR/combined.cover" ]; then
    tail -n +2 "$f" >> "$COVERDIR/combined.cover"
  fi
done

# Generate HTML if requested
if [ "$HTML_MODE" = true ]; then
  go tool cover -html="$COVERDIR/combined.cover" -o "$COVERDIR/coverage.html"
  echo "HTML report: $COVERDIR/coverage.html"
fi

# Calculate per-package coverage
echo ""
echo "=== Coverage Summary ==="
echo ""

BELOW_THRESHOLD=false

for pkg in "${PACKAGES[@]}"; do
  pkg_name=$(basename "$pkg")
  if [ -f "$COVERDIR/${pkg_name}.cover" ]; then
    coverage=$(go tool cover -func="$COVERDIR/${pkg_name}.cover" 2>/dev/null | grep "total:" | awk '{print $3}' | tr -d '%')
    if [ -n "$coverage" ]; then
      # Check threshold for both modes
      if (( $(echo "$coverage < $THRESHOLD" | bc -l) )); then
        BELOW_THRESHOLD=true
      fi
      if [ "$CI_MODE" = true ]; then
        echo "coverage:${pkg_name}:${coverage}%"
      else
        if (( $(echo "$coverage < $THRESHOLD" | bc -l) )); then
          echo "  $pkg_name: ${coverage}% (BELOW ${THRESHOLD}% target)"
        else
          echo "  $pkg_name: ${coverage}%"
        fi
      fi
    else
      echo "  $pkg_name: 0.0% (no tests or no coverage data)"
      BELOW_THRESHOLD=true
    fi
  fi
done

echo ""

# Total coverage
total_coverage=$(go tool cover -func="$COVERDIR/combined.cover" 2>/dev/null | grep "total:" | awk '{print $3}' | tr -d '%')
if [ -n "$total_coverage" ]; then
  if [ "$CI_MODE" = true ]; then
    echo "coverage:total:${total_coverage}%"
  else
    echo "Total coverage: ${total_coverage}%"
  fi
fi

# Warn but don't fail (per Phase 2 decision)
if [ "$BELOW_THRESHOLD" = true ]; then
  if [ "$CI_MODE" = true ]; then
    echo "::warning::Some packages below ${THRESHOLD}% coverage target"
  else
    echo ""
    echo "WARNING: Some packages below ${THRESHOLD}% coverage target"
  fi
fi

echo ""
echo "Coverage data: $COVERDIR/"
