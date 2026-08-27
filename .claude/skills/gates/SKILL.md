---
name: gates
description: Use when about to claim work is done, before committing or pushing, or when asked whether something broke — runs this repository's quality gates at the right level and reports what was verified and what was not.
---

# Gates

Run the repository's gates and report the result honestly.

The gates are shell scripts under `scripts/gates/`, not commands embedded here.
A human, CI and you all run the same implementation, so a green here means the
same thing a green in CI means.

## Pick the level

| level | command | cost | when |
|---|---|---|---|
| 1 | `scripts/gates/all.sh --level 1` | seconds | while iterating; before every commit |
| 2 | `scripts/gates/all.sh --level 2` | minutes | before claiming done; **before any push** |
| 3 | `scripts/gates/all.sh --level 3` | tens of minutes | anything touching relay / claim / proof |

Narrow with `PKG=miner` while iterating. Widen before you conclude: a package
that passes alone can still break its callers.

Level 2 is the floor for "done". Level 1 proves the tree compiles and is tidy;
it proves nothing about behaviour.

**Level 3 is not optional for the money path.** A change to relay, claim, proof,
settlement or metering is not verified by unit tests. If `live.sh` does not
exist yet, `all.sh` prints it as NOT RUN — say so in your report rather than
letting level 2's green stand in for it.

## Read the result

Each gate's last line is its verdict; `all.sh` prints a summary. Three outcomes,
and they are not the same:

- **PASS, every gate ran** — the only one that supports "verified".
- **PASS with gates not run** — a tool was missing or a gate does not exist.
  Report it as "X passed, Y not run", never as green. "I found nothing" and "I
  did not look" must not produce the same signal.
- **FAIL** — go to the `gate-triage` skill. Do not re-run hoping for green.

### FIRST read WHICH revision it measured — a verdict without one is not evidence

`all.sh` opens and closes with `revision <sha> (<branch>) -- clean tree`. Read
it, and compare it against the branch you believe you are gating. Three things
measured on 2026-08-27, all in one session:

- **A review agent checked out another branch in this worktree mid-run.** The
  gate reported `PASS level 2` for a branch that was not the one under work, and
  uncommitted changes rode the checkout so a commit landed on the wrong branch.
  The revision line was the only thing that said so.
- **The dirty-tree mark does not cover it.** The tree was clean at both ends.
- **The HEAD-moved warning does not cover it either**: it compares the two
  samples, so leaving a branch and returning before the summary is invisible to
  it. That run went red for another reason — `[build failed]`, the signature of a
  tree that changed under the compiler — and the cause was only readable because
  the gate now KEEPS the raw output of a failure under
  `scripts/localonly/_state/gate-evidence/`.

**So: ONE worktree, ONE job at a time.** Never launch an agent that checks out
branches while gating, and never leave uncommitted changes with one in flight. If
work must run in parallel, it goes in a separate `git worktree`.

## Never through a pipe

`cmd | tail` reports **tail's** exit code: a red suite and a 600-second deadlock
both read as success. Run the gate bare, to a FILE, and read `$?` separately —
and that applies to your own verification of a fix as much as to the gate, since
the status that reaches you belongs to the last command, which is not the one you
are testing.

Measured in this session, repeatedly: `all.sh --level 1 2>&1 | grep -E '...'`
reads grep's status. It happened to be green every time, which is exactly why the
habit survives — the first red it hides is the one that matters.

And when you match against a gate's output, **strip the colour escapes in a
separate statement after capturing the status**: the runner prints
`<red>FAIL<reset> level 2`, so no fixed-string match spans it (budgetkit paid for
that one, 2026-08-26).

## The one-line test for whether this ran

Name the gate that did NOT run, in those exact words, before anything else.
Then state what you ran and what it proved. Not "tests pass" but "level 2 passed:
suite, race and coverage, whole tree". If you ran `PKG=`, say which package. If
a gate was skipped, name it and say the coverage is incomplete.

Never claim a gate's result you did not observe. Run it and read the output.

## The adversarial pass, when it is delegated to agents

**HOW to dispatch them is not here** — `item` owns it, beside the council step
that does the dispatching: launch unnamed, propose the cost first, give the
question the structure, treat the output as a claim. One rule, one artefact.

What belongs to the VERDICT is this: **a fan-out can die WHOLE.** On 2026-08-19 a
ten-agent review returned zero findings because every agent went idle without
reporting, and it happened again with five agents on another model. A dead
fan-out does not yield fewer findings, it yields **none** — while the branch reads
as reviewed.

- **An agent that goes idle has NOT reported.** Ask it by name once. If it goes
  idle again with nothing, stop asking and do the pass yourself; a second round of
  reminders buys nothing and reads like progress.
- **If the fan-out dies, the review did NOT run.** Redo it with commands and say
  so in the report, in those words.
- **The command-based pass is legitimate** — that same day it found five real
  defects, three of them introduced by the session itself, and on 2026-08-26 it
  found a gate reporting its units in one mode and not the other. It is weaker in
  exactly one nameable way: it cannot attack what the author did not think of. So
  name the angles in writing BEFORE starting — removed behaviour, cross-file
  callers, double-counted metrics, language pitfalls, efficiency — and the pass is
  a checklist rather than an improvisation.

## Related

- `test-teeth` — before trusting a passing test, prove it can fail.
- `gate-triage` — when a gate goes red.
