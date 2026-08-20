#!/usr/bin/env bash
#
# Shared plumbing for the repository's quality gates.
#
# Source this, do not execute it. Every gate under scripts/gates/ reports
# through these helpers so that a human, CI and an agent all read the same
# output and can rely on the same contract:
#
#   * exit 0 means the gate passed, non-zero means it failed;
#   * the LAST line is the verdict, so a caller that keeps only the tail still
#     learns the outcome;
#   * a gate REPORTS, it never FIXES. A gate that rewrites files hides the
#     failure it just found, and in a pre-commit context it rewrites the tree
#     after git has already snapshotted the index.
#   * a gate has no side effects: it does not touch git, the index, or the
#     working tree.
#
# A missing tool is a SKIP, not a pass. "I found nothing" and "I did not look"
# must never produce the same signal, so a skip is printed loudly and, unlike a
# pass, is counted and reported in the verdict.

# Colour only when a terminal is going to read it. CI logs and piped output get
# plain text, which keeps grep and log viewers honest.
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    GATE_BOLD=$'\033[1m'
    GATE_RED=$'\033[0;31m'
    GATE_GREEN=$'\033[0;32m'
    GATE_YELLOW=$'\033[0;33m'
    GATE_RESET=$'\033[0m'
else
    GATE_BOLD=''
    GATE_RED=''
    GATE_GREEN=''
    GATE_YELLOW=''
    GATE_RESET=''
fi
readonly GATE_BOLD GATE_RED GATE_GREEN GATE_YELLOW GATE_RESET

gate_failed=0
gate_skipped=0

# gate_step <name> -- announce the check about to run.
gate_step() {
    printf '%s==>%s %s\n' "$GATE_BOLD" "$GATE_RESET" "$1"
}

# gate_pass <message>
gate_pass() {
    printf '%s  ok%s   %s\n' "$GATE_GREEN" "$GATE_RESET" "$1"
}

# gate_fail <message> -- marks the whole gate failed.
gate_fail() {
    printf '%s  FAIL%s %s\n' "$GATE_RED" "$GATE_RESET" "$1"
    gate_failed=$((gate_failed + 1))
}

# gate_skip <message> -- a check that could not run. Counted, and named in the
# verdict, so an absent tool can never be mistaken for a clean result.
gate_skip() {
    printf '%s  skip%s %s\n' "$GATE_YELLOW" "$GATE_RESET" "$1"
    gate_skipped=$((gate_skipped + 1))
}

# gate_detail <text> [max_lines] -- indent captured output under a finding.
gate_detail() {
    local text="$1" max="${2:-20}"
    [ -z "$text" ] && return 0
    printf '%s\n' "$text" | head -"$max" | sed 's/^/           /'
}

# gate_verdict <gate name> -- print the final line and exit with the gate's
# status. Call this as the last statement of every gate script.
gate_verdict() {
    local name="$1"
    echo
    if [ "$gate_failed" -ne 0 ]; then
        printf '%sFAIL%s %s: %d check(s) failed' \
            "$GATE_RED" "$GATE_RESET" "$name" "$gate_failed"
        [ "$gate_skipped" -ne 0 ] && printf ', %d skipped' "$gate_skipped"
        printf '\n'
        exit 1
    fi
    if [ "$gate_skipped" -ne 0 ]; then
        printf '%sPASS%s %s (%d check(s) skipped -- not verified)\n' \
            "$GATE_GREEN" "$GATE_RESET" "$name" "$gate_skipped"
        exit 0
    fi
    printf '%sPASS%s %s\n' "$GATE_GREEN" "$GATE_RESET" "$name"
    exit 0
}

# gate_repo_root -- cd to the repository root so a gate behaves the same
# wherever it is invoked from.
gate_repo_root() {
    local root
    root="$(git rev-parse --show-toplevel 2>/dev/null)" || {
        printf '%sFAIL%s not inside a git repository\n' "$GATE_RED" "$GATE_RESET"
        exit 1
    }
    cd "$root" || exit 1
}

# gate_pkg_target -- the package pattern a gate should act on, honouring PKG.
# PKG=miner narrows to ./miner/...; unset means the whole tree.
# gate_pkg_normalized -- PKG with the shapes shell completion produces
# stripped (trailing slash, leading ./), so every dispatch below sees the same
# name. Without this, PKG=cache/ targeted ./cache/... but missed the
# sequential-parallelism branch and raced the shared miniredis fixture.
gate_pkg_normalized() {
    local p="${PKG:-}"
    p="${p%/}"
    p="${p#./}"
    printf '%s' "$p"
}

gate_pkg_target() {
    local p
    p="$(gate_pkg_normalized)"
    if [ -n "$p" ]; then
        printf './%s/...' "$p"
    else
        printf './...'
    fi
}

# gate_parallelism -- the -p/-parallel flags for a `go test` run, as a shell
# word list on stdout.
#
# Two packages must run sequentially when targeted on their own: cache and
# miner share a single miniredis fixture across their tests, so running them in
# parallel races the fixture rather than testing the code. A whole-tree run
# keeps parallelism because `go test` already isolates packages in separate
# processes -- the contention is between tests INSIDE one package, which
# -parallel governs.
#
# Keep this list in one place: it used to live inline in the Makefile's `test`
# and `test_miner` targets with different values in each.
gate_parallelism() {
    case "$(gate_pkg_normalized)" in
    cache | miner) printf -- '-p 1 -parallel 1' ;;
    *) printf -- '-p 4 -parallel 4' ;;
    esac
}

# gate_unexplained_shortfall SENT BILLED ANNOUNCED_DROPS
#
# Prints how many relays went missing WITHOUT the miner saying so. A relay that
# arrives after its tree was sealed, or after its claim window closed, cannot be
# paid and there is nothing to recover -- but it must have been counted, and the
# counter is what makes it acceptable. Anything left over is the silent loss the
# live gate exists to catch.
#
# Negative results are clamped to 0: more announced drops than missing relays
# means the counter also caught traffic outside this measurement, which is not
# evidence of a loss.
gate_unexplained_shortfall() {
    local sent="${1:-0}" billed="${2:-0}" dropped="${3:-0}" unexplained
    unexplained=$(( sent - billed - dropped ))
    if [ "$unexplained" -lt 0 ]; then
        unexplained=0
    fi
    printf '%s' "$unexplained"
}
