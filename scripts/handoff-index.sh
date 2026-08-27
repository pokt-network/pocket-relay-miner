#!/usr/bin/env bash
# Indexes the session hand-overs in scripts/localonly/ and says which one is
# CANONICAL -- the one a session must read before doing anything else.
#
# Canonical is a DECISION, not a date. The newest hand-over is frequently not
# the one that governs: a session can close by writing a fresh hand-over that
# defers to an older one, and on 2026-08-19 that is exactly what happened
# (HANDOFF-2026-08-18-r1.md was superseded by -08-19-r1 while both stayed on
# disk). So this script never guesses by mtime. It reads the declaration out of
# the queue and REFUSES to answer when there is none.
#
# The contract, one line in scripts/localonly/QUEUE-deep-cleanup.md:
#
#     **Handoff CANÓNICO: `HANDOFF-2026-08-26-r1.md`**
#
# Why a script and not a convention: 24 hand-overs accumulated, and which one
# governed lived in a sentence a human had to keep rewriting. It stopped being
# rewritten, sessions read a stale one, and a chain of branches sat unpushed for
# days because each session wrote its finding into a hand-over nobody actioned.
#
# Run: scripts/handoff-index.sh
# Exit: 0 ok · 1 no canonical declared, or it does not exist · 2 the pointer is
#       stale (a hand-over is newer than the canonical one).
set -euo pipefail

cd "$(dirname "$0")/.."

LOCAL_DIR="${HANDOFF_DIR:-scripts/localonly}"
QUEUE="${HANDOFF_QUEUE:-$LOCAL_DIR/QUEUE-deep-cleanup.md}"

red() { printf '\033[31m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

if [ ! -d "$LOCAL_DIR" ]; then
    red "ERROR: $LOCAL_DIR does not exist."
    echo "Working documents live there and it is gitignored; nothing to index."
    exit 1
fi

# The hand-overs, newest mtime last. mtime is used ONLY to order the listing and
# to detect a stale pointer -- never to pick the canonical one.
mapfile -t handoffs < <(find "$LOCAL_DIR" -maxdepth 1 -name 'HANDOFF-*.md' -printf '%T@ %p\n' \
    | sort -n | awk '{print $2}')

if [ ${#handoffs[@]} -eq 0 ]; then
    red "ERROR: no HANDOFF-*.md in $LOCAL_DIR."
    exit 1
fi

if [ ! -f "$QUEUE" ]; then
    red "ERROR: the queue is missing: $QUEUE"
    echo "Without it nothing declares which hand-over is canonical, and this"
    echo "script does not guess. Point HANDOFF_QUEUE at the right file."
    exit 1
fi

# Only the declaration counts. Accents and case vary between sessions, so match
# loosely on the label and strictly on the backticked filename.
canonical="$(grep -oiE 'handoff[[:space:]]+CAN[OÓ]NICO:[[:space:]]*`[^`]+`' "$QUEUE" \
    | tail -1 | grep -oE '`[^`]+`' | tr -d '`' || true)"

if [ -z "$canonical" ]; then
    red "ERROR: no hand-over is declared canonical in $QUEUE."
    echo
    echo "This is NOT the same as 'the newest one wins'. Add the line, in the"
    echo "queue's header, naming the hand-over that governs:"
    echo
    echo '    **Handoff CANÓNICO: `HANDOFF-<date>-<r>.md`**'
    echo
    echo "Hand-overs present, oldest first (NOT a ranking):"
    printf '  %s\n' "${handoffs[@]#"$LOCAL_DIR"/}"
    exit 1
fi

canonical_path="$LOCAL_DIR/$canonical"
if [ ! -f "$canonical_path" ]; then
    red "ERROR: the queue declares '$canonical' canonical, and it does not exist."
    echo "Either the file was renamed and the pointer was not, or the pointer is a typo."
    exit 1
fi

# A hand-over newer than the canonical one is the exact shape of the failure this
# script exists for: somebody closed a session and did not move the pointer.
newer=()
for h in "${handoffs[@]}"; do
    [ "$h" = "$canonical_path" ] && continue
    [ "$h" -nt "$canonical_path" ] && newer+=("${h#"$LOCAL_DIR"/}")
done

echo "CANONICAL (read this first, the rest is evidence of how we got here):"
green "  $canonical"
sed -n '1,3p' "$canonical_path" | sed 's/^/    | /'
echo
echo "Historical (${#handoffs[@]} total, newest first):"
for ((i = ${#handoffs[@]} - 1; i >= 0; i--)); do
    h="${handoffs[$i]}"
    [ "$h" = "$canonical_path" ] && continue
    printf '  %-34s %s\n' "$(basename "$h")" "$(date -r "$h" '+%Y-%m-%d %H:%M')"
done

if [ ${#newer[@]} -gt 0 ]; then
    echo
    yellow "STALE POINTER: ${#newer[@]} hand-over(s) are NEWER than the canonical one:"
    printf '  %s\n' "${newer[@]}"
    echo
    echo "Either one of those should be canonical and the queue was not updated,"
    echo "or the older one really does still govern -- in which case say so in the"
    echo "queue, so the next session does not have to guess."
    exit 2
fi
