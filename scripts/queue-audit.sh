#!/usr/bin/env bash
#
# Audit the work queue against reality, so a DISCARDED item stops reading like a
# pending one.
#
# Why this exists (Jorge, 2026-08-31): "por eso no logro tener el stack completo,
# porque volver a traerme items descartados". A session read the queue, reported
# issue #25 as open work, and it had been closed that same day -- along with #43,
# #7 and #8. The queue is the only durable record of what is pending, and nothing
# was checking it against the world, so every session re-litigated closed work and
# the stack never converged.
#
# It reports; it never edits. Exit 1 when something needs a human decision.
set -u

QUEUE="${QUEUE:-scripts/localonly/QUEUE-deep-cleanup.md}"
RED=$'\033[31m'; YEL=$'\033[33m'; GRN=$'\033[32m'; BOLD=$'\033[1m'; OFF=$'\033[0m'

[ -f "$QUEUE" ] || { printf 'queue not found: %s\n' "$QUEUE" >&2; exit 2; }

if ! command -v gh >/dev/null 2>&1; then
    printf '%sSKIP%s gh is not installed -- issue state NOT verified (this is not a pass)\n' "$YEL" "$OFF"
    exit 2
fi

# Issue state, once, rather than per reference.
states="$(gh issue list --state all --limit 200 --json number,state,title 2>/dev/null)" || {
    printf '%sSKIP%s could not reach GitHub -- issue state NOT verified (this is not a pass)\n' "$YEL" "$OFF"
    exit 2
}

printf '%s== queue items whose issue is already closed%s\n' "$BOLD" "$OFF"

QUEUE="$QUEUE" STATES="$states" python3 - <<'PY'
import json, os, re, sys

queue = open(os.environ["QUEUE"], encoding="utf-8").read()
state = {i["number"]: (i["state"], i["title"]) for i in json.loads(os.environ["STATES"])}

RED, YEL, GRN, OFF = "\033[31m", "\033[33m", "\033[32m", "\033[0m"

# One item = a "# NN. Title" heading and everything up to the next one.
# The queue uses TWO heading shapes and missing one is a silent false green:
# the ordered list at the top is "## 1." / "## 1-bis.", the appended findings are
# "# 64.". Measured 2026-08-31: matching only the second reported 16 items clear
# and skipped item 4, which was the stale #25 reference this script exists for.
parts = re.split(r"^#{1,2} (\d+(?:-bis)?)\. (.*)$", queue, flags=re.M)
items = [(parts[i], parts[i + 1], parts[i + 2]) for i in range(1, len(parts), 3)]

# The admission rule, and it is the whole point of the list: an entry is only
# work if somebody MEASURED that it is still alive. Prose is not evidence.
EVIDENCE = re.compile(
    r"[A-Za-z0-9_./-]+\.(go|sh|json|yaml|yml|md):\d+"   # file:line
    r"|`[0-9a-f]{7,40}`"                                  # a commit
    r"|verificad|medid|VERIFICADO|MEDIDO"
    r"|Decisi[oó]n de Jorge|Pedido de Jorge|Ruling de Jorge", re.I)
SETTLED = re.compile(r"CERRADO|DESCARTADO|DIFERIDO|SE IGNORA|MUERTO|NO REPRODUCIBLE", re.I)

stale, unmarked, ok = [], [], 0
for num, title, body in items:
    refs = {int(n) for n in re.findall(r"#(\d{1,4})\b", body) if int(n) in state}
    closed = sorted(n for n in refs if state[n][0] == "CLOSED")
    settled = bool(SETTLED.search(body))
    if closed and not settled:
        stale.append((num, title, closed))
    elif not EVIDENCE.search(body) and not settled:
        unmarked.append((num, title))
    else:
        ok += 1

for num, title, closed in stale:
    print(f"  {RED}STALE{OFF} item {num}: {title[:56]}")
    for n in closed:
        print(f"          references #{n}, which is CLOSED -- {state[n][1][:52]}")

if not stale:
    print(f"  {GRN}ok{OFF}   no item claims pending work on a closed issue")

print(f"\n\033[1m== entries with no measurement behind them\033[0m")
if unmarked:
    for num, title in unmarked:
        print(f"  {YEL}NO EVIDENCE{OFF} item {num}: {title[:56]}")
    print("\n  An item with no status reads as pending to the next session. That is")
    print("  how discarded work comes back. Mark it, or say why it cannot be marked.")
else:
    print(f"  {GRN}ok{OFF}   every entry carries evidence or is marked settled")

print(f"\n{len(items)} item(s): {ok} clear, {len(stale)} stale, {len(unmarked)} unmarked")
sys.exit(1 if (stale or unmarked) else 0)
PY
