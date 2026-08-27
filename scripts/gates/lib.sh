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
# Checks that ran but measured nothing -- see gate_nothing_measured.
gate_vacuous=0

# gate_step <name> -- announce the check about to run.
gate_step() {
    printf '%s==>%s %s\n' "$GATE_BOLD" "$GATE_RESET" "$1"
}

# gate_pass <message>
gate_pass() {
    printf '%s  ok%s   %s\n' "$GATE_GREEN" "$GATE_RESET" "$1"
}

# gate_fail <message> -- marks the whole gate failed.
#
# ONLY WORKS CALLED DIRECTLY. gate_fail increments $gate_failed IN THE
# CURRENT SHELL. A helper function invoked via command substitution --
# anything called as `x="$(some_helper ...)"` -- runs in a SUBSHELL, and a
# gate_fail called from inside it increments a copy of $gate_failed that dies
# with the subshell. gate_verdict, back in the real shell, never sees it: the
# gate prints PASS and the failure is gone with no error, no trace.
#
# A helper that needs to fail must instead `return` non-zero and let the
# CALLER (which is not inside a subshell) check that and call gate_fail
# itself. gate_settlement_breakdown is the worked example: it returns 1 on a
# jq parse failure, and live.sh checks that exit status (captured via
# `x="$(...)"; rc=$?` BEFORE the value is consumed by anything else, since
# `read var <<<"$(...)"` throws the substitution's exit status away in favor
# of read's own) before calling gate_fail itself (review 2026-08-20, found
# while fixing exactly this in gate_settlement_breakdown).
#
# The pure helpers here today (gate_unexplained_shortfall, gate_served_
# shortfall) dodge this because they return via echo and never call
# gate_fail, so the trap is latent, not yet triggered -- waiting for the
# next helper that tries to do both at once.
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

# gate_nothing_measured <message> -- the gate RAN and exercised nothing.
#
# This is a third outcome, not a flavour of pass or fail. A gate whose infra is
# absent, whose package list matched nothing, or whose measurement came back
# empty has not disproved anything -- and reporting it as PASS is how "verified"
# comes to mean "did not look". all.sh maps the exit status below onto its
# NOT RUN list, so a vacuous gate can never be counted as coverage.
#
# Measured in the pair 2026-08-26: budgetkit's `make test-infra` reported success
# with its Postgres unreachable and 374 tests then accused the product of a
# defect that was the harness talking to another tool's database; this
# repository's coverage.sh printed "coverage run clean" with an empty total,
# because the number was optional in the message.
gate_nothing_measured() {
    printf '%s  NOTHING%s %s\n' "$GATE_YELLOW" "$GATE_RESET" "$1"
    gate_vacuous=$((gate_vacuous + 1))
}

# gate_exercised <label> <count> -- record HOW MUCH this check exercised.
#
# Writes to a FILE, not to a shell variable, and that is the whole point. The
# counters above (gate_failed, gate_skipped, gate_vacuous) only work when
# incremented in the CURRENT shell -- see the warning on gate_fail -- so a helper
# that MEASURES something and is therefore invoked as `x="$(helper ...)"` cannot
# report through them: it would increment a copy that dies with the subshell. A
# `printf >>` from inside a subshell writes to the same inode, and the
# orchestrator reads the file after the gate has exited.
#
# The default is the safe one: a gate that never calls this leaves the file empty,
# all.sh sums zero, and the gate is reported NOT RUN. Forgetting to report cannot
# produce green. Credit where due -- this design came from budgetkit
# (`cleanup-skills-2`, 2026-08-26), arguing from this file's own subshell warning.
# Two CLASSES, and the class is mandatory because the same zero means opposite
# things: `coverage` is what the gate OBSERVED (packages, files, billed relays) and
# zero is RED; `findings` is what it FOUND (races, drifts, failures) and zero is the
# good result. Measured in budgetkit 2026-08-26: one screen printed DATA RACE=0,
# FAIL=0 and ok=0 -- two good, one a defect. An unclassified counter is a decision
# left implicit by whoever wrote it, so it errors visibly instead.
gate_exercised() {
    case "${1:-}" in
    coverage | findings) ;;
    *)
        printf '%s  FAIL%s gate_exercised: first argument must be coverage|findings, got %q\n' \
            "$GATE_RED" "$GATE_RESET" "${1:-}"
        gate_failed=$((gate_failed + 1))
        return 2
        ;;
    esac
    [ -n "${GATE_EXERCISED_FILE:-}" ] || return 0
    printf '%s %s %s\n' "$1" "$2" "${3:-0}" >>"$GATE_EXERCISED_FILE"
}

# gate_pkg_passes <json-file> -- packages that PASSED, counted from `go test -json`.
#
# NOT `grep -c '^ok '` on the human output. That prose is not a contract: budgetkit
# measured 2026-08-26 that a runner changing its own output turned a perfectly green
# run into "0 packages", and its two gates disagreed with each other by a single
# space (`^ok  ` against `^ok   `). A counter that parses another tool's prose makes
# the anti-vacuity ledger itself vacuous.
gate_pkg_passes() {
    python3 -c '
import json, sys
n = 0
for line in open(sys.argv[1]):
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("Action") == "pass" and "Test" not in e:
        n += 1
print(n)
' "$1" 2>/dev/null || printf '0'
}

# gate_json_output <json-file> -- reconstruct the human text from the same file.
gate_json_output() {
    python3 -c '
import json, sys
for line in open(sys.argv[1]):
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("Action") == "output":
        sys.stdout.write(e.get("Output", ""))
' "$1" 2>/dev/null || true
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
    # A gate that RAN and measured nothing is RED, not amber (Jorge, 2026-08-26:
    # we always expect to find something, for better or worse, never nothing). Only
    # an ABSENT gate stays NOT RUN -- a file that never executed has no measurement
    # that could be zero.
    if [ "$gate_vacuous" -ne 0 ]; then
        printf '%sFAIL%s %s: %d check(s) ran and measured nothing\n' \
            "$GATE_RED" "$GATE_RESET" "$name" "$gate_vacuous"
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
#
# EventApplicationOverserviced is filtered by the same min-session-end-height
# window as EventClaimSettled -- an events file spans the whole scan range a
# caller handed to scan_settlement_events, not just this run's load window,
# so an unfiltered $over would let a stale overservicing event from a
# PREVIOUS run report as "overservicing occurred" for this one (review
# 2026-08-21, same failure shape the window filter above already guards
# against for $settled).
#
# Returns non-zero, on stdout the same all-zero row as the empty-file case,
# when jq itself fails (schema change, malformed JSON, jq missing). The
# CALLER must check that exit status and gate_fail -- this function cannot
# call gate_fail itself and have it count: every caller invokes it via
# $(...) to capture the TSV, and a command substitution runs in a subshell,
# so a gate_failed increment made in here would vanish when the subshell
# exits (review 2026-08-20, MEDIUM). The empty-events-file case above is
# NOT an error and stays a silent, correct zero; jq actually failing IS
# one, and used to be indistinguishable from it -- this ONLY existed to
# swallow the (already-handled) empty-file case, and swallowed jq failures
# with it.
gate_settlement_breakdown() {
    local events_file="$1" minend="${2:-0}"
    [ -s "$events_file" ] || { printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0'; return 0; }
    jq -rs --argjson minend "$minend" '
        def num: tostring | gsub("[^0-9]"; "") | if . == "" then 0 else tonumber end;
        [ .[] | select(.type == "pocket.tokenomics.EventClaimSettled")
              | select((.attrs.session_end_block_height // "0" | num) >= $minend) ] as $settled
      | [ .[] | select(.type == "pocket.tokenomics.EventApplicationOverserviced")
              | select((.attrs.session_end_block_height // "0" | num) >= $minend) ] as $over
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
    ' "$events_file" || { printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0'; return 1; }
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

# gate_provenance -- name the revision this run measured. Printed when the run
# opens and again beside the verdict, so a log can be checked against the HEAD
# actually being proposed.
#
# Measured 2026-08-27: a level 3 PASS log contained no line naming a commit, so
# tying that run to `13a7362` needed a comparison of file timestamps against
# `git log`. The rule "the gates must be green on the exact HEAD" was
# procedural only, and it had already failed that week -- green on `c0ff4d2`
# while HEAD was `13a7362`.
#
# A dirty tree is announced, not failed: gates are run mid-edit on purpose. But
# the code that ran is then not the code the SHA names, so the run is not
# attributable to that commit and the line must say so.
gate_provenance() {
    local head branch
    head="$(git rev-parse --short HEAD 2>/dev/null)" || head='(none)'
    branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)" || branch='(none)'
    if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
        printf '%srevision%s %s (%s) -- %sDIRTY TREE: this run is NOT attributable to that commit%s\n' \
            "$GATE_BOLD" "$GATE_RESET" "$head" "$branch" "$GATE_YELLOW" "$GATE_RESET"
    else
        printf '%srevision%s %s (%s) -- clean tree\n' \
            "$GATE_BOLD" "$GATE_RESET" "$head" "$branch"
    fi
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
