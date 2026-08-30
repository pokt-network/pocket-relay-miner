---
name: start
description: Use at the opening of a session, before any work — verifies the machine against what the hand-over claims, reports every divergence, and does not let work begin on an unread queue or an unstated plan.
---

# Start

A session inherits claims, not facts. This turns them into facts before anything
is built on them.

## Step 0 — VERIFY, first, always

```
scripts/session-start-check.sh
```

It states the branch, HEAD, tree, remote divergence, the namespace it looked in,
and every application pod, and it FLAGS the states no hand-over can make
acceptable. Exit 1 = something flagged. Exit 2 = the canonical pointer is broken.

**A status API is not evidence.** Measured 2026-08-22: Tilt reported
`update: ok` / `runtime: ok` on SEVENTEEN resources with **not one application
pod alive** — a `tilt up --stream` had been running 5h11m against a cluster that
had been deleted and recreated. And measured 2026-08-26, pods reported Running
while serving a binary four days old. The check that catches that one is the pod's
start time against the newest commit touching Go; restarts never caught it.

**And the pod's start time is not enough either.** Measured 2026-08-29, after a
15-link rebase: the relayer pods were 4 minutes old and their image tag had not
changed across a `tilt trigger`, yet the binary inside had been rebuilt — the
image layout itself moved, from `/app/pocket-relay-miner` to
`/usr/local/bin/pocket-relay-miner`. Age said fresh, the tag said unchanged, and
only the CONTENT told the truth.

So verify by content, and **discover the path, never hard-code it**. A `grep`
against a stale path returns "no such file or directory", and a `2>/dev/null`
turns that into "the marker is absent" — indistinguishable from "the binary is
the old one", and it reads as a legitimate wait. Ask the container
(`command -v <binary>`), grep what it answers, and make "could not measure" print
differently from "does not match". Use two markers, not one: a string the current
tree HAS and a string only the other branch has, so presence and absence are both
asserted.

## Step 0a — a mid-session crash re-enters HERE, and what survives is the repo

A session that died does not reopen as a new one. Run Step 0, then read
`git log` and `git status` for what was in flight, and `scripts/localonly/` for
what it was writing. **Only what is in the repo survives**: measured 2026-08-26,
a session suspended four days lost the state file it had put in the harness
scratchpad, because the restart cleared it.

## Step 0b — does the previous session's hand-over cover its own close?

```
scripts/handoff-index.sh
```

Canonical is a DECISION, not a date. Exit 2 means a hand-over is NEWER than the
declared canonical one — a pointer nobody moved, which is how a chain of branches
sat unpushed while each session wrote its finding into a document nobody actioned.

## Step 0c — say where this session's files go, before producing one

`scripts/localonly/` (gitignored), never `/tmp` and never the harness scratchpad.
Measured 2026-08-26: a session suspended four days lost the pod-UID snapshot that
was going to prove nothing rotated during a 25-minute run, because the restart
cleared the scratchpad. The rule already existed and was broken anyway, because
the harness instruction pushes the other way.

## Step 0d — what a FRESH CLONE does not have, and this session probably does

Say it before reading anything, because the steps below name files a clone does
not contain, and a session that assumes they are there will look for them or,
worse, assume their content.

Measured 2026-08-26:

| a clone gets | this machine also has |
|---|---|
| 9 skill files, 10 gate scripts, 10 operator docs | **175 entries in `scripts/localonly/`** — 25 hand-overs, 14 findings/plans/runbooks, and the QUEUE — all gitignored |
| | **70 memory files** in the harness home, outside the repo entirely |
| | 2 non-skill entries under `.claude/`, ignored by `.gitignore:29` |

**The canonical hand-over, the queue and the memory index — the three artifacts
steps 0b and 1 send you to — are ALL invisible to a clone.** So a session opening
without them is not a session with less context; it is a session that cannot run
steps 0b, 1 and 3d at all, and must say so instead of reporting them done.

The rules travel; the history does not. That asymmetry is deliberate — operator
data never reaches a tracked file — and it is exactly why this step exists rather
than being obvious.

## Step 1 — read, in this order

1. The canonical hand-over that Step 0b named.
2. `scripts/localonly/QUEUE-deep-cleanup.md` — the index of everything pending.

## Step 2 — recite the gates, at the START

Say the levels and their cost out loud (`gates` has them), and say **which the
work you are about to do will require**. A change to relay, claim, proof,
settlement or metering needs level 3, and level 3 needs the localnet up — that is
your problem to solve, not the human's: check docker, kind, the cluster and the
registry, and bring Tilt up yourself.

## Step 3 — surface the environment traps

Read the queue's traps section. The ones that bite most often here: the test Redis
on 6399 dies on its own; port 8180 can come up IPv6-only after `tilt up`; **one
live case at a time** — two gates in parallel once produced exactly double the
leaves and looked like a bug; and never edit `.go` with a live run in flight.

**This machine runs two products.** `default` is this repository, `budgetkit-dev`
is not. Before reporting that something broke, re-run the command with the scope
stated — `-n`, the path, the context. An implicit default is a hypothesis.
And before killing any process, read its `cwd` and parent: measured twice on
2026-08-26, once for a `go test -race` and once for a `tilt up`, both belonging to
another repository.

## Step 3b — sweep the standing rulings against the TREE, not against their text

A ruling's status line rots faster than anything else, because the session that
APPLIES it is a later one that never goes back to mark it. Take each open ruling
and check the code, not the sentence next to it.

Measured 2026-08-26, twice in one file: the queue said "NOT DONE" about a fix
another session had already committed, and the auto-loaded memory index named a
hand-over that had stopped being canonical — in two separate lines.

## Step 3c — carry forward the gates that did NOT run

A level that was not run in the previous session is an ITEM with a position, not
a footnote. "PASS with gates not run" is not green.

## Step 3d — read the whole queue's headings, and say which you did not read

Naming what you skipped is the difference between a scoped session and a session
that will rediscover something already written down.

## Step 4 — survey the open work, SIZE it, and agree the order BEFORE executing

Jorge's standing rule: *"quizás debemos explorar los topics, buscar el mejor
orden y luego empezar el trabajo realmente."* Following a discovery mid-flight is
still right when it earns it; what must not be emergent is the ORDER.

Restate the working order in one line after two or three reorderings and get it
confirmed. That is this skill's job to ask for, not the human's to remember.

## Step 4b — build the session PLAN, and do not start without it

Written, and confirmed before the first edit. Without it the human ends up
supervising the work instead of deciding on it — measured on 2026-08-26, when two
sessions each built what they thought had been agreed and neither noticed that one
repository held four of the eight agreed skills and the other held eight. A plan
printed at the start makes that gap visible in the first minute instead of the
third hour.

## Step 4f — are SUBAGENTS allowed in this run? Ask now, write the answer down

**This is a precondition of `item`, not a detail**: its branch-1 step invokes the
council, and the council IS subagents. The answer is not constant — a session can
carry a standing instruction that forbids dispatching them, and it will not
announce itself; it surfaces mid-item, exactly when the council is needed.

Live instance, this repository, 2026-08-26: the session's own instructions
forbade dispatching agents unless the human asked, so the council was PROPOSED
with its cost rather than run, and the adversarial pass was done with commands.
That was the right call and it cost nothing because the answer was known at the
start. Found mid-item, it costs the item.

Write the answer in the plan, next to the cost, in both cases.

## Step 4c — every item runs through `item`

Including the small one. The step that gets skipped for small items is the step
that was written because a small item cost four commits.

## The one-line test for whether this ran

The reply names the divergence between the hand-over and the machine -- or says
there was none, **having run the commands**. Then the canonical hand-over, the
flags Step 0 raised and what was done about each, the gate level this session's work will require, and the agreed order.
