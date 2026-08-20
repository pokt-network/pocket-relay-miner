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

# gate_settlement_breakdown <events file> <min session end height>
#
# Sums the settlement numbers the chain reports, as TSV:
#   claims relays estimated claimed settled minted overloss deflation over_events spend_limit
#
# It lives here, with a self-test, because it parses MONEY out of event
# attributes whose values arrive JSON-encoded (a uint64 field reads as the
# string "\"4\"", a coin as "\"1000upokt\""), and a silent parse failure would
# read as a clean zero -- indistinguishable from "this did not happen".
#
# relays and estimated are NOT the same quantity and must not be summed
# together: relays counts the leaves in the submitted tree, i.e. only the relays
# whose hash matched the service's mining difficulty, while estimated is that
# count scaled back up by the difficulty multiplier. They coincide only at base
# difficulty.
gate_settlement_breakdown() {
    local events_file="$1" minend="${2:-0}"
    [ -s "$events_file" ] || { printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0'; return 0; }
    jq -rs --argjson minend "$minend" '
        def num: tostring | gsub("[^0-9]"; "") | if . == "" then 0 else tonumber end;
        [ .[] | select(.type == "pocket.tokenomics.EventClaimSettled")
              | select((.attrs.session_end_block_height // "0" | num) >= $minend) ] as $settled
      | [ .[] | select(.type == "pocket.tokenomics.EventApplicationOverserviced") ] as $over
      | [ ($settled | length),
          ([$settled[].attrs.num_relays // 0 | num] | add // 0),
          ([$settled[].attrs.num_estimated_relays // 0 | num] | add // 0),
          ([$settled[].attrs.claimed_upokt // 0 | num] | add // 0),
          ([$settled[].attrs.settled_upokt // 0 | num] | add // 0),
          ([$settled[].attrs.minted_upokt // 0 | num] | add // 0),
          ([$settled[].attrs.overservicing_loss_upokt // 0 | num] | add // 0),
          ([$settled[].attrs.deflation_loss_upokt // 0 | num] | add // 0),
          ($over | length),
          ([$over[] | select((.attrs.spend_limit_exceeded // "" | tostring) | test("true"))] | length) ]
      | @tsv
    ' "$events_file" 2>/dev/null || printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0'
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
# sequential-parallelism branch below.
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
# Three packages must run sequentially when targeted on their own: cache, miner
# and relayer mutate PROCESS-WIDE state in place. All three read Prometheus
# counters through testutil.ToFloat64 as before/after deltas, and cache also
# reassigns the L1 TTL globals (serviceCacheL1TTL and its four siblings),
# restoring them in t.Cleanup. Two such tests running at once read each other's
# writes.
#
# The miner testify suites add a third: SetupTest clears the whole SUITE-WIDE
# key prefix, so a sibling test running at the same time loses its keys.
#
# These flags are NOT the guard, and must not be read as one. They only apply
# when PKG names one of these packages; the whole-tree run that every gate and
# CI actually perform falls through to the -p 4 -parallel 4 branch below. The
# guard is TestNoTestParallelWhereStateIsShared in internal/conventions, which
# fails on a t.Parallel() in any of them regardless of flags. These flags are
# belt to that check'"'"'s braces, and cost nothing today because no test in the
# three calls t.Parallel().
#
# This comment used to say the packages shared one miniredis fixture, and a
# previous edit replaced that with "they never did". Both were wrong. Most
# tests did call miniredis.Run() individually, but the miner testify suites
# (RedisSMSTTestSuite, SupplierClaimerTestSuite) really did share one instance
# per suite and FlushAll it between tests -- which is exactly the hazard that
# survived the migration as the suite-wide DeletePrefix above.
#
# Keep this list in one place: it used to live inline in the Makefile's `test`
# and `test_miner` targets with different values in each.
gate_parallelism() {
    case "$(gate_pkg_normalized)" in
    cache | miner | relayer) printf -- '-p 1 -parallel 1' ;;
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

# gate_served_shortfall EXPECTED SERVED
#
# Prints how many relays a cell asked for and did not get. A relay that never
# reached the relayer never reaches a claim either, so scoring a run against
# what SUCCEEDED instead of what was REQUESTED hides that loss by construction:
# the settlement assert would then compare billed against the reduced number and
# pass. Kept as a function so the rule cannot quietly decay back into "more than
# zero is fine".
gate_served_shortfall() {
    local expected="${1:-0}" served="${2:-0}" missing
    missing=$(( expected - served ))
    if [ "$missing" -lt 0 ]; then
        missing=0
    fi
    printf '%s' "$missing"
}
