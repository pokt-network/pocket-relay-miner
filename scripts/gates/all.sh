#!/usr/bin/env bash
#
# Run the repository's gates, in order, up to a level.
#
# Usage:
#   scripts/gates/all.sh                  # level 2 (the default: everything
#                                         #  that runs without a cluster)
#   scripts/gates/all.sh --level 1        # static only -- seconds
#   scripts/gates/all.sh --level 3        # + live validation on Tilt
#   scripts/gates/all.sh --keep-going     # run every gate even after a failure
#   PKG=miner scripts/gates/all.sh        # narrow to one package
#
# Levels are cost tiers, not importance tiers:
#
#   1  static     seconds        gofmt, build, vet, lint, tracked files
#   2  tests      minutes        test suite, race detector, coverage
#   3  live       tens of min.   relays mined and settled on-chain, on Tilt
#
# Fail-fast by default, because the levels are ordered by dependency as well as
# cost: a tree that does not build cannot produce a meaningful test result, so
# running level 2 after a level 1 failure buys noise. --keep-going when you want
# the full picture in one pass.

set -uo pipefail

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

gate_repo_root

readonly GATES_DIR="scripts/gates"

level=2
keep_going=0

while [ $# -gt 0 ]; do
    case "$1" in
    --level)
        if [ $# -lt 2 ]; then
            printf -- '--level requires a value (1, 2 or 3)\n' >&2
            exit 2
        fi
        level="$2"
        shift 2
        ;;
    --level=*)
        level="${1#--level=}"
        shift
        ;;
    --keep-going)
        keep_going=1
        shift
        ;;
    -h | --help)
        sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        printf 'unknown argument: %s\n' "$1" >&2
        exit 2
        ;;
    esac
done

case "$level" in
1 | 2 | 3) ;;
*)
    printf 'level must be 1, 2 or 3 (got: %s)\n' "$level" >&2
    exit 2
    ;;
esac

# Gate list per level, in dependency order.
gates=("$GATES_DIR/static.sh")
if [ "$level" -ge 2 ]; then
    gates+=("$GATES_DIR/tests.sh" "$GATES_DIR/race.sh" "$GATES_DIR/coverage.sh")
fi
if [ "$level" -ge 3 ]; then
    gates+=("$GATES_DIR/live.sh")
fi

failed_gates=()
skipped_gates=()

for gate in "${gates[@]}"; do
    name="$(basename "$gate" .sh)"

    if [ ! -x "$gate" ]; then
        # A gate that is planned but not written yet must announce itself. An
        # absent gate silently dropped from the run is the difference between
        # "verified" and "did not look".
        printf '\n%s### %s -- NOT AVAILABLE%s\n' "$GATE_YELLOW" "$name" "$GATE_RESET"
        printf '    %s is missing or not executable; this level is NOT fully covered\n' "$gate"
        skipped_gates+=("$name")
        continue
    fi

    printf '\n%s### %s%s\n' "$GATE_BOLD" "$name" "$GATE_RESET"
    if "$gate"; then
        continue
    fi

    failed_gates+=("$name")
    if [ "$keep_going" -eq 0 ]; then
        printf '\n%sStopping at the first failed gate.%s Re-run with --keep-going for the full picture.\n' \
            "$GATE_YELLOW" "$GATE_RESET"
        break
    fi
done

# ---------------------------------------------------------------------------
echo
printf '%s=== summary (level %s) ===%s\n' "$GATE_BOLD" "$level" "$GATE_RESET"

if [ "${#skipped_gates[@]}" -ne 0 ]; then
    printf '%sNOT RUN:%s %s\n' "$GATE_YELLOW" "$GATE_RESET" "${skipped_gates[*]}"
fi

if [ "${#failed_gates[@]}" -ne 0 ]; then
    printf '%sFAIL%s level %s -- failed: %s\n' \
        "$GATE_RED" "$GATE_RESET" "$level" "${failed_gates[*]}"
    exit 1
fi

if [ "${#skipped_gates[@]}" -ne 0 ]; then
    printf '%sPASS%s level %s, with gates not run (see above) -- coverage is incomplete\n' \
        "$GATE_GREEN" "$GATE_RESET" "$level"
    exit 0
fi

printf '%sPASS%s level %s -- every gate ran and passed\n' \
    "$GATE_GREEN" "$GATE_RESET" "$level"
exit 0
