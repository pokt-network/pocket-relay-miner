#!/usr/bin/env bash
#
# Measure the machine at the start of a session, and flag the states that are
# always wrong -- so a session never inherits a claim it did not check.
#
# It reports FACTS, in the shape a hand-over states them, rather than parsing the
# hand-over's prose: the comparison is the reader's job, and prose is not a
# contract. What it does decide on its own are the states that no hand-over can
# make acceptable -- a dirty tree at hour zero, pods that are restarting, a
# cluster with no pods at all.
#
# Why this exists, all measured 2026-08-22 and 2026-08-26 in this repository:
#   * Tilt's API reported update:ok / runtime:ok on SEVENTEEN resources with not
#     one application pod alive -- a `tilt up --stream` had been running 5h11m
#     against a cluster that had been deleted and recreated. A status API is not
#     evidence; a pod list is.
#   * Pods reported Running while serving a binary four days old, because the
#     file watch had not seen the edits.
#   * A branch's remote sat SIX commits behind its local, so "pushed" in a
#     hand-over meant a different tree than the one on disk.
#
# Run: scripts/session-start-check.sh
# Exit: 0 nothing flagged · 1 at least one always-wrong state · 2 the canonical
#       hand-over pointer is broken (handoff-index.sh said so).
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1

RED=$'\033[31m'; YELLOW=$'\033[33m'; GREEN=$'\033[32m'; BOLD=$'\033[1m'; OFF=$'\033[0m'
flagged=0
flag() { printf '%s  FLAG%s %s\n' "$RED" "$OFF" "$1"; flagged=$((flagged + 1)); }
fact() { printf '  %-22s %s\n' "$1" "$2"; }
head2() { printf '\n%s== %s%s\n' "$BOLD" "$1" "$OFF"; }

# --------------------------------------------------------------- the hand-over
head2 "canonical hand-over"
if [ -x scripts/handoff-index.sh ]; then
    scripts/handoff-index.sh | sed -n '1,5p'
    idx_rc=${PIPESTATUS[0]}
    if [ "$idx_rc" -eq 1 ]; then
        printf '%s  the canonical pointer is broken -- see above%s\n' "$RED" "$OFF"
        exit 2
    fi
    [ "$idx_rc" -eq 2 ] && flag "a hand-over is NEWER than the declared canonical one (stale pointer)"
else
    flag "scripts/handoff-index.sh is missing; nothing states which hand-over governs"
fi

# --------------------------------------------------------------------- the repo
head2 "repository"
branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
fact branch "$branch"
fact HEAD "$(git rev-parse --short HEAD 2>/dev/null)"

dirty="$(git status --porcelain | grep -c . || true)"
if [ "$dirty" -ne 0 ]; then
    # At hour zero this is somebody else's unfinished work, or a session that
    # closed without committing. Either way it is not yours to assume.
    flag "$dirty uncommitted change(s) already in the tree -- find out whose before editing"
    git status --short | head -10 | sed 's/^/           /'
else
    fact tree clean
fi

upstream="$(git rev-parse --abbrev-ref '@{upstream}' 2>/dev/null || true)"
if [ -n "$upstream" ]; then
    read -r behind ahead <<<"$(git rev-list --left-right --count "$upstream...HEAD" 2>/dev/null)"
    fact upstream "$upstream"
    if [ "${ahead:-0}" -ne 0 ] || [ "${behind:-0}" -ne 0 ]; then
        # Not a flag: a stack of unpushed work is this repository's normal state.
        # It is reported because "pushed" in a hand-over then names another tree.
        printf '  %-22s %s\n' "divergence" "${ahead:-0} ahead, ${behind:-0} behind ${YELLOW}(the remote is NOT what is on disk)${OFF}"
    else
        fact divergence "in sync"
    fi
else
    fact upstream "none -- this branch was never pushed"
fi

# ------------------------------------------------------------------- the cluster
head2 "cluster (pods, not a status API)"
if ! command -v kubectl >/dev/null 2>&1; then
    printf '  kubectl absent -- cluster state NOT checked\n'
else
    ctx="$(kubectl config current-context 2>/dev/null || echo '<none>')"
    fact context "$ctx"
    # The namespace is STATED and passed explicitly. An implicit default is a
    # hypothesis, not a scope: measured 2026-08-26, `kubectl get pods` with no
    # -n showed another repository's session this repository's pods and it came
    # within a minute of reporting that a stack had been deleted. Two products
    # share this cluster -- default and budgetkit-dev.
    ns="${SESSION_CHECK_NS:-default}"
    fact namespace "$ns (explicit; other namespaces are not this repo's)"
    pods="$(kubectl get pods -n "$ns" --request-timeout=5s --no-headers 2>/dev/null || true)"
    if [ -z "$pods" ]; then
        printf '  %sno pods in this namespace -- if a hand-over says the stack is up, it is wrong%s\n' \
            "$YELLOW" "$OFF"
        printf '  (this is the 5h11m failure: Tilt reported ok against a recreated cluster)\n'
    else
        fact pods "$(printf '%s\n' "$pods" | grep -c .)"
        not_running="$(printf '%s\n' "$pods" | awk '$3 != "Running" && $3 != "Completed"' | grep -c . || true)"
        [ "$not_running" -ne 0 ] && flag "$not_running pod(s) not Running:" && \
            printf '%s\n' "$pods" | awk '$3 != "Running" && $3 != "Completed"' | sed 's/^/           /'
        # Restarts are a FACT, not a flag: a pod that died and came back may be
        # serving the same image, and flagging fifteen of them every morning is
        # how a signal stops being read.
        restarting="$(printf '%s\n' "$pods" | awk '$4 + 0 > 0' | grep -c . || true)"
        [ "$restarting" -ne 0 ] && fact "pods restarted" "$restarting of $(printf '%s\n' "$pods" | grep -c .)"

        # This is the check that catches the measured failure -- pods reported
        # Running while serving a binary four days old. Restarts never caught it;
        # the build being older than the code does. If a pod started BEFORE the
        # newest commit that touched Go, the code it runs cannot include that
        # commit, whatever any status API says.
        newest_go="$(git log -1 --format=%ct -- '*.go' 2>/dev/null)"
        if [ -n "$newest_go" ]; then
            fact "newest .go commit" "$(date -d "@$newest_go" '+%Y-%m-%d %H:%M')"
            stale=0
            while read -r name started image; do
                [ -z "$started" ] && continue
                started_ts="$(date -d "$started" +%s 2>/dev/null || echo 0)"
                if [ "$started_ts" -gt 0 ] && [ "$started_ts" -lt "$newest_go" ]; then
                    stale=$((stale + 1))
                    printf '           %s started %s, image %s\n' \
                        "$name" "$(date -d "$started" '+%m-%d %H:%M')" "${image##*:}"
                fi
            done < <(kubectl get pods -n "$ns" --request-timeout=5s \
                -o 'custom-columns=N:.metadata.name,S:.status.startTime,I:.spec.containers[0].image' \
                --no-headers 2>/dev/null | grep -E 'miner|relayer')
            [ "$stale" -gt 0 ] && flag "$stale application pod(s) started BEFORE the newest .go commit -- they cannot be running this tree"
        fi
    fi
fi

# --------------------------------------------------------------------- verdict
echo
if [ "$flagged" -ne 0 ]; then
    printf '%s%d state(s) flagged.%s Resolve or name each one before starting work.\n' "$RED" "$flagged" "$OFF"
    exit 1
fi
printf '%snothing flagged.%s The facts above are still yours to compare against the hand-over.\n' \
    "$GREEN" "$OFF"
