#!/usr/bin/env bash
#
# Self-test for the pure helpers in lib.sh. Runs inside the static gate, where
# it costs milliseconds. It exists because gate_unexplained_shortfall decides
# whether the live gate excuses a missing relay: an assertion that has never
# been red is decoration, and this one guards money.

set -uo pipefail

# A pre-commit hook runs with GIT_DIR exported, and from a LINKED WORKTREE that
# value is ABSOLUTE -- so every `git` this file runs in a throwaway directory
# operates on the parent repository instead. Measured 2026-08-29: committing from
# a linked worktree made the fixture below `git init` and `git commit -m first`
# against the real repo, which took whatever was staged and moved `main` onto a
# commit called "first". Reproduced in isolation with an absolute GIT_DIR
# exported; the same run also left core.bare=true on the parent, which was
# observed but not reproduced. Unset before any fixture touches git.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY

# shellcheck source=scripts/gates/lib.sh
. "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

failures=0

expect() {
    local want="$1" got="$2" what="$3"
    if [ "$want" != "$got" ]; then
        printf '  FAIL %s: want %s, got %s\n' "$what" "$want" "$got" >&2
        failures=$((failures + 1))
    fi
}

# sent billed dropped -> unexplained
expect 0 "$(gate_unexplained_shortfall 72 72 0)"  "nothing missing"
expect 0 "$(gate_unexplained_shortfall 72 66 6)"  "every missing relay announced"
expect 3 "$(gate_unexplained_shortfall 72 66 3)"  "half announced, half silent"
expect 6 "$(gate_unexplained_shortfall 72 66 0)"  "NOTHING announced -- the loss this gate exists to catch"
expect 0 "$(gate_unexplained_shortfall 72 66 9)"  "more announced than missing is not evidence of a loss"
expect 0 "$(gate_unexplained_shortfall 0 0 0)"    "empty run"
expect 5 "$(gate_unexplained_shortfall 5 0 0)"    "everything lost, nothing said"

# expected served -> missing
expect 0  "$(gate_served_shortfall 60 60)" "everything asked for was served"
expect 20 "$(gate_served_shortfall 60 40)" "a third never made it -- scoring against 40 would hide it"
expect 60 "$(gate_served_shortfall 60 0)"  "nothing served"
expect 0  "$(gate_served_shortfall 60 61)" "more than asked is not a shortfall"
expect 0  "$(gate_served_shortfall 0 0)"   "empty cell"

# gate_settlement_breakdown: parses money out of JSON-encoded event attributes,
# where a silent parse failure returns a clean-looking zero. A zero from this
# helper is read as "overservicing did not occur"; if it can also mean "the parse
# broke", the report is worse than nothing.
sb_fixture="$(mktemp)"
prov_repo=''
# One EXIT trap for the whole file: a second bare `trap ... EXIT` REPLACES this
# one rather than adding to it, and lib_test runs inside the pre-commit hook,
# where a Ctrl-C mid-commit is ordinary.
trap 'rm -f "$sb_fixture"; [ -n "$prov_repo" ] && rm -rf "$prov_repo"' EXIT
cat >"$sb_fixture" <<'FIXTURE'
{"height":"100","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"100\"","num_relays":"\"4\"","num_estimated_relays":"\"4\"","claimed_upokt":"\"1000upokt\"","settled_upokt":"\"1000upokt\"","minted_upokt":"\"1000upokt\"","overservicing_loss_upokt":"\"0\"","deflation_loss_upokt":"\"0\""}}
{"height":"100","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"100\"","num_relays":"\"6\"","num_estimated_relays":"\"12\"","claimed_upokt":"\"3000upokt\"","settled_upokt":"\"2000upokt\"","minted_upokt":"\"1800upokt\"","overservicing_loss_upokt":"\"1000\"","deflation_loss_upokt":"\"200\""}}
{"height":"100","type":"pocket.tokenomics.EventApplicationOverserviced","attrs":{"session_end_block_height":"\"100\"","spend_limit_exceeded":"true"}}
{"height":"50","type":"pocket.tokenomics.EventClaimSettled","attrs":{"session_end_block_height":"\"50\"","num_relays":"\"99\"","num_estimated_relays":"\"99\"","claimed_upokt":"\"9999upokt\"","settled_upokt":"\"9999upokt\"","minted_upokt":"\"9999upokt\"","overservicing_loss_upokt":"\"7\"","deflation_loss_upokt":"\"7\""}}
{"height":"50","type":"pocket.tokenomics.EventApplicationOverserviced","attrs":{"session_end_block_height":"\"50\"","spend_limit_exceeded":"true"}}
FIXTURE

# The height-50 claim is BEFORE the window and must be excluded; leaving it in
# would inflate every number and, worse, invent 7 uPOKT of overservicing from a
# previous run. The height-50 EventApplicationOverserviced is the same trap on
# the OTHER counter: $settled already filtered by session_end_block_height,
# $over did not, so a stale overservicing event from a previous run used to
# report as "overservicing occurred THIS run" no matter how old it was
# (review 2026-08-21).
expect "$(printf '2\t10\t16\t4000\t3000\t2800\t1000\t200\t1\t1')" \
    "$(gate_settlement_breakdown "$sb_fixture" 100)" \
    "settlement breakdown, window at 100 -- both the stale claim AND the stale overservicing event must be excluded"

# estimated (16) differs from relays (10) on purpose: summing them together, or
# reading one for the other, is the difficulty-multiplier confusion this helper
# exists to keep visible.
expect "$(printf '3\t109\t115\t13999\t12999\t12799\t1007\t207\t2\t2')" \
    "$(gate_settlement_breakdown "$sb_fixture" 0)" \
    "no window: the older claim and the older overservicing event are both included"

expect "$(printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0')" \
    "$(gate_settlement_breakdown /nonexistent/events.jsonl 0)" \
    "a missing events file yields zeros, not an error"

# A NON-empty but malformed events file (schema change, truncated write, jq
# missing) is a real parse failure, not the "no events yet" case above -- and
# must be distinguishable from it. Before this test existed, both cases
# returned the exact same all-zero row with no exit-status signal, so
# live.sh's overservicing check read "jq broke" as "overservicing did not
# occur" (MEDIUM-3, review 2026-08-20).
bad_fixture="$(mktemp)"
printf 'not json at all\n' >"$bad_fixture"
bad_out="$(gate_settlement_breakdown "$bad_fixture" 0 2>/dev/null)"
bad_rc=$?
rm -f "$bad_fixture"
expect "$(printf '0\t0\t0\t0\t0\t0\t0\t0\t0\t0')" "$bad_out" \
    "malformed input still prints the zero row, so a numeric read downstream does not blow up"
if [ "$bad_rc" -eq 0 ]; then
    printf '  FAIL malformed input: want a non-zero exit status from gate_settlement_breakdown, got 0 -- the caller cannot tell this apart from a genuinely quiet run\n' >&2
    failures=$((failures + 1))
fi

# gate_provenance: the line that ties a green log to a commit. The dirty-tree
# branch is the load-bearing one -- a clean-looking "revision <sha>" on a tree
# that does not match that sha is exactly the false attribution the helper was
# added to prevent (2026-08-27). Run against a throwaway repo, because the
# repository this test lives in is dirty precisely when someone is editing it.
prov_repo="$(mktemp -d)"
# gpgsign is set globally on at least one developer machine (verified
# 2026-08-27), and a fixture commit that waits on pinentry, or fails without a
# cached agent, would leave a directory that is not a repository -- against
# which the "clean tree" and "names HEAD" assertions below would both pass for
# the wrong reason. So: signing off, and the setup's exit status is CHECKED.
# The redirection goes INSIDE the substitution. Written as `)" 2>&1` it is a
# simple command made of an assignment plus a redirection, so the redirection
# applies to THIS shell -- fd 2 pointed at its own fd 1 -- and never reaches
# the subshell: the capture came back empty while git's error text leaked to
# stdout. Measured 2026-08-27 with `git definitely-not-a-flag`.
prov_setup_err="$( {
    cd "$prov_repo" || exit 1
    git init -q . &&
        git -c user.email=gate@test -c user.name=gate -c commit.gpgsign=false \
            commit -q --allow-empty -m 'first'
} 2>&1 )"
prov_setup_rc=$?
if [ "$prov_setup_rc" -ne 0 ]; then
    printf '  FAIL gate_provenance fixture: could not build the throwaway repo (rc=%s): %s\n' \
        "$prov_setup_rc" "$prov_setup_err" >&2
    failures=$((failures + 1))
fi

prov_head="$(cd "$prov_repo" && git rev-parse --short HEAD 2>/dev/null)"
if [ -z "$prov_head" ]; then
    # Without this the glob `*""*` below matches EVERY string, so the assertion
    # that the line names HEAD would pass against any output at all.
    printf '  FAIL gate_provenance fixture: no HEAD in the throwaway repo, so the assertions below cannot bite\n' >&2
    failures=$((failures + 1))
    prov_head='<no-head>'
fi

prov_clean="$(cd "$prov_repo" && gate_provenance)"
case "$prov_clean" in
*"clean tree"*) ;;
*)
    printf '  FAIL gate_provenance on a clean tree: want a line saying "clean tree", got %s\n' \
        "$prov_clean" >&2
    failures=$((failures + 1))
    ;;
esac

case "$prov_clean" in
*"$prov_head"*) ;;
*)
    printf '  FAIL gate_provenance: the line does not name HEAD (%s), so it cannot tie the run to a commit: %s\n' \
        "$prov_head" "$prov_clean" >&2
    failures=$((failures + 1))
    ;;
esac

: >"$prov_repo/uncommitted"
prov_dirty="$(cd "$prov_repo" && gate_provenance)"
# `DIRTY TREE`, not the `NOT attributable` the two branches share: matching the
# shared phrase would stay green if the helper regressed to reporting a dirty
# tree as UNKNOWN, which is the neighbouring branch (review, 2026-08-27).
case "$prov_dirty" in
*"DIRTY TREE"*) ;;
*)
    printf '  FAIL gate_provenance on a DIRTY tree: want the run marked DIRTY TREE, got %s\n' \
        "$prov_dirty" >&2
    failures=$((failures + 1))
    ;;
esac
# git unreadable is a THIRD state, not a synonym for clean: an empty
# `status --porcelain` is what BOTH "no changes" and "the command failed"
# produce, and only this assertion tells them apart. Found by review on
# 2026-08-27, when the helper printed `revision (none) ((none)) -- clean tree`
# from outside a repository.
#
# TMPDIR is not guaranteed to sit outside every repository -- a $HOME that is
# itself a dotfiles repo is the ordinary case -- and git would then discover
# THAT repo and print a revision, failing this assertion for a reason that has
# nothing to do with the helper. GIT_CEILING_DIRECTORIES stops the upward
# search, but only for an entry STRICTLY ABOVE the working directory: measured
# 2026-08-27, a ceiling equal to the working directory was ignored and the
# parent repo was found anyway, while the parent as ceiling worked. Hence
# dirname, plus an explicit precondition so a fixture that is still inside a
# repo says so instead of failing as if the helper were broken.
prov_norepo_dir="$(mktemp -d)"
prov_norepo_ceiling="$(dirname "$prov_norepo_dir")"
if (cd "$prov_norepo_dir" && GIT_CEILING_DIRECTORIES="$prov_norepo_ceiling" \
    git rev-parse --show-toplevel >/dev/null 2>&1); then
    printf '  FAIL gate_provenance fixture: %s is inside a git repository even with a ceiling at %s, so the no-repo case cannot be exercised here\n' \
        "$prov_norepo_dir" "$prov_norepo_ceiling" >&2
    failures=$((failures + 1))
fi
prov_norepo="$(cd "$prov_norepo_dir" && GIT_CEILING_DIRECTORIES="$prov_norepo_ceiling" gate_provenance)"
rmdir "$prov_norepo_dir"
case "$prov_norepo" in
*"NOT attributable"*) ;;
*)
    printf '  FAIL gate_provenance outside a repository: want the run marked NOT attributable, got %s\n' \
        "$prov_norepo" >&2
    failures=$((failures + 1))
    ;;
esac

rm -rf "$prov_repo"
prov_repo=''

# gate_keep_evidence: the raw output of a FAILING gate must survive the gate.
# Asserted on the CONTENT, not just on a path being printed -- a path naming an
# empty or missing file is the same "looked nowhere" signal in another costume.
keep_src="$(mktemp)"
printf 'first line\nTHE ACTUAL CAUSE\n' >"$keep_src"
keep_out="$(gate_keep_evidence "$keep_src" libtest-probe)"
keep_dest="$(printf '%s' "$keep_out" | grep -oE 'scripts/localonly/[^ ]+')"
if [ -z "$keep_dest" ] || [ ! -f "$keep_dest" ]; then
    printf '  FAIL gate_keep_evidence: no readable file behind the reported path: %s\n' \
        "$keep_out" >&2
    failures=$((failures + 1))
elif ! grep -q 'THE ACTUAL CAUSE' "$keep_dest"; then
    printf '  FAIL gate_keep_evidence: the kept file does not carry the output it was given (%s)\n' \
        "$keep_dest" >&2
    failures=$((failures + 1))
fi
rm -f "$keep_src"
[ -n "$keep_dest" ] && rm -f "$keep_dest"

# before after -> delta. The reset case is the one that matters: a negative
# delta would be subtracted from a shortfall and would EXCUSE a silent loss.
expect 0  "$(gate_counter_delta 0 0)"      "counter never moved"
expect 6  "$(gate_counter_delta 10 16)"    "counter advanced normally"
expect 16 "$(gate_counter_delta 0 16)"     "counter started at zero"
expect 4  "$(gate_counter_delta 10 4)"     "RESET: after < before, the honest delta is what it has seen since"
expect 0  "$(gate_counter_delta 10 0)"     "reset with no traffic since -- zero, never -10"

# sent billed dropped -> the verdict BOTH the settlement wait and the final
# assertion of live.sh read. The L3 of df5441c (2026-09-11) is the 59/60 row:
# with its redelivered drops no longer announced, nothing explains the missing
# relay, so the wait keeps it pending and the end reports it LOST.
expect settled   "$(gate_exact_cell_state 60 60 0)" "every relay billed"
expect settled   "$(gate_exact_cell_state 60 60 3)" "billed in full: drops elsewhere do not matter"
expect over      "$(gate_exact_cell_state 60 61 0)" "billed more than sent"
expect accounted "$(gate_exact_cell_state 60 58 2)" "every missing relay announced: the wait may stop"
expect accounted "$(gate_exact_cell_state 60 58 9)" "more announced than missing"
expect short     "$(gate_exact_cell_state 60 59 0)" "the L3 of df5441c: one relay missing, nothing announced"
expect short     "$(gate_exact_cell_state 60 57 2)" "announced drops explain only part of it"
expect short     "$(gate_exact_cell_state 5 0 0)"   "everything lost, nothing said"
expect settled   "$(gate_exact_cell_state 0 0 0)"   "empty cell"

# One verdict, two readers: the wait (services_pending) and the final assertion
# must both call gate_exact_cell_state, or they drift apart again -- a wait that
# stops on a rule the assertion does not accept, or waits on one it does.
live_sh="$(dirname "${BASH_SOURCE[0]}")/live.sh"
# shellcheck disable=SC2016 # the pattern is the literal call text, not an expansion
uses="$(grep -c '$(gate_exact_cell_state ' "$live_sh" 2>/dev/null || true)"
expect 2 "${uses:-0}" "live.sh must read gate_exact_cell_state in the wait AND in the final assertion"

# The reasons live.sh accepts as announced, matched the way PromQL matches a
# label regex (anchored): a redelivered copy must never explain a missing relay,
# and neither may a relay dropped because its tree was already sealed.
reasons="$(sed -n "s/^announced_drop_reasons='\(.*\)'$/\1/p" "$live_sh")"
accepts() { [[ "$1" =~ ^(${reasons})$ ]] && printf yes || printf no; }
expect no  "$(accepts session_sealed)"                  "a sealed tree with nothing late to wait for is a loss, not an announcement"
expect yes "$(accepts claim_window_closed)"             "a relay past its claim window is announced"
expect no  "$(accepts session_sealed_redelivered)"      "a redelivered copy is not an announcement"
expect no  "$(accepts claim_window_closed_redelivered)" "nor past the window"

# gate_expected_timeout_regime: window_blocks x block_time_seconds against the
# SDK ceiling (589.99s). Localnet's own clock knob (localnet.block_time_seconds
# in tilt_config.yaml) moves this boundary at runtime -- 30s stays under it,
# 60s (mainnet's clock) goes over it -- so a fixed "ceiling must be 0"
# expectation is wrong at exactly the clock this gate needs to pass under.
expect window  "$(gate_expected_timeout_regime 10 30)" "10 blocks x 30s = 300s, well under the ceiling"
expect window  "$(gate_expected_timeout_regime 10 58)" "580s, just under"
expect ceiling "$(gate_expected_timeout_regime 10 59)" "590s > 589.99s, just over -- the boundary itself"
expect ceiling "$(gate_expected_timeout_regime 10 60)" "600s, mainnet's clock"

# The ceiling gate_expected_timeout_regime hard-codes (600000/10000/10 ms) is a
# SECOND, independent copy of tx.DefaultTxTimeoutMax's own three numbers
# (txTimeoutHardCeiling / txTimeoutSafetyMargin / txNonceSpread in
# tx/tx_client.go) -- not a re-derivation from anything read at runtime. Extract
# BOTH copies with sed and compare them: tx_window_timeout_test.go already pins
# the Go side as a Go value, but nothing before this test would have caught the
# two copies saying different numbers.
tx_client_go="$(dirname "${BASH_SOURCE[0]}")/../../tx/tx_client.go"
go_hard_ceiling_min="$(sed -n 's/^\ttxTimeoutHardCeiling = \([0-9]*\) \* time\.Minute$/\1/p' "$tx_client_go")"
go_safety_margin_s="$(sed -n 's/^\ttxTimeoutSafetyMargin = \([0-9]*\) \* time\.Second$/\1/p' "$tx_client_go")"
go_nonce_spread_ms="$(sed -n 's/^const txNonceSpread = \([0-9]*\) \* time\.Millisecond$/\1/p' "$tx_client_go")"

lib_sh="$(dirname "${BASH_SOURCE[0]}")/lib.sh"
lib_hard_ceiling_ms="$(sed -n 's/^[[:space:]]*local hard_ceiling_ms=\([0-9]*\)$/\1/p' "$lib_sh")"
lib_safety_margin_ms="$(sed -n 's/^[[:space:]]*local safety_margin_ms=\([0-9]*\)$/\1/p' "$lib_sh")"
lib_nonce_spread_ms="$(sed -n 's/^[[:space:]]*local nonce_spread_ms=\([0-9]*\)$/\1/p' "$lib_sh")"

if [ -z "$go_hard_ceiling_min" ] || [ -z "$go_safety_margin_s" ] || [ -z "$go_nonce_spread_ms" ] ||
    [ -z "$lib_hard_ceiling_ms" ] || [ -z "$lib_safety_margin_ms" ] || [ -z "$lib_nonce_spread_ms" ]; then
    printf '  FAIL timeout-ceiling drift check: could not extract one of the six numbers (go: %s/%s/%s, lib: %s/%s/%s) -- either side''s source changed shape and the sed pattern no longer matches\n' \
        "$go_hard_ceiling_min" "$go_safety_margin_s" "$go_nonce_spread_ms" \
        "$lib_hard_ceiling_ms" "$lib_safety_margin_ms" "$lib_nonce_spread_ms" >&2
    failures=$((failures + 1))
else
    expect "$(( go_hard_ceiling_min * 60 * 1000 ))" "$lib_hard_ceiling_ms" \
        "hard ceiling: tx_client.go's 10 * time.Minute vs lib.sh's copy"
    expect "$(( go_safety_margin_s * 1000 ))" "$lib_safety_margin_ms" \
        "safety margin: tx_client.go's 10 * time.Second vs lib.sh's copy"
    expect "$go_nonce_spread_ms" "$lib_nonce_spread_ms" \
        "nonce spread: tx_client.go's 10 * time.Millisecond vs lib.sh's copy"
fi

if [ "$failures" -ne 0 ]; then
    printf 'lib_test: %s failure(s)\n' "$failures" >&2
    exit 1
fi
printf 'lib_test: all cases pass\n'
