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

**Level 1 does NOT run `internal/conventions`, and that is the trap.** Those
checks -- no new bare `go` statements, no new `time.Sleep` in tests, no
`sync.Map`, keys through the KeyBuilder -- live in the `tests` section, so they
run at level 2 and later. A commit made on a green level 1 can carry a violation
of a rule this repository enforces mechanically, and the first thing that says
so is a level 2 or 3 run, after the commit exists. Measured 2026-09-03: a
`time.Sleep` in a test written that same session rode through a level-1-green
commit and was caught by `TestNoNewSleepsInTests` at level 3, tens of minutes
later. If a commit adds or edits a `_test.go`, or adds a goroutine, run
`go test -tags test ./internal/conventions/` before it -- it costs seconds.

**Level 3 is not optional for the money path.** A change to relay, claim, proof,
settlement or metering is not verified by unit tests. If `live.sh` does not
exist yet, `all.sh` prints it as NOT RUN — say so in your report rather than
letting level 2's green stand in for it.

## WHO runs it: never the session that wrote the code

When two sessions work one tree, the gate is not a turn to share. It belongs to
the session that did NOT write the change, and it does not rotate.

The reason is not the resource conflict -- that is a side effect. **A gate run by
the author measures what the author exercised; the same gate run by the other
measures the tree.** Sharing the turn leaves verification in the hands of the
verified half the time, which is the one thing two sessions exist to prevent.

Measured 2026-09-08, and it paid for itself the same afternoon: an author handed
over a commit with `go build`, `go vet` and their own targeted tests green -- all
three correctly run. The full package under the gate died with a SIGSEGV in a
goroutine, taking the whole test binary with it and reporting **zero failed
tests**, so the failure named nothing. Targeted tests could not have seen it.

The split, and the second half matters as much as the first:

- **The verifier owns**: `make gate LEVEL=*` entire, the run on a clean HEAD after
  every commit, and the injections that verify committed work.
- **The author keeps**: `go build`, `go vet`, a `go test -run <Test>` for the test
  being written, and injections against their own uncommitted tree. None of those
  is a gate -- they are how you avoid committing something that does not compile.

Leaving the author blind is not the goal, and an author who is told only "you do
not run gates" will run them anyway.

Same day, same tree: two gates share ONE Redis container on a fixed port, and the
`down` of whichever finishes first deletes it under the other -- 20 failures, all
connection-refused, in a run whose result was therefore NULL and not red. One
runner makes that impossible. The ownership marker does not save you: it answers
"did I start it?", and ownership TRANSFERS to whoever recreates a container that
died on its own.

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

**And a `;` list hides it exactly like a pipe does.** Measured 2026-08-28:
`cascade.sh ... > log 2>&1; echo "EXIT=$?"` reports the status of the `echo`,
which is always 0 — the `$?` inside the string is expanded and printed, but the
LIST's own exit code is the last command's. A cascade that aborted on a red gate
was read as a success, and a defect was filed against the script for returning 0
when the script returns 1 correctly. Verified by running it and reading `$?` with
nothing after it. Capture the status in a variable on its OWN line, then echo the
variable:

```sh
scripts/gates/all.sh --level 2 > "$log" 2>&1
rc=$?                      # nothing between the command and this line
echo "gate rc=$rc"
```

**Reading the rule is not obeying it, and this is the measured proof.** On
2026-08-29 a session with this whole section in context wrote
`git rebase ... 2>&1 | tail -30` then `rc=$?` — tail's status — and later
`PROM_WINDOW=20m gate.sh --preflight-only 2>&1 | head -3` then `echo "rc: $?"`,
which reported 0 for a check that had not run at all and briefly stood as
evidence that a new validation worked. Both were caught by re-measuring, not by
remembering. So the habit that actually holds is mechanical, not vigilant: when a
command's status matters, **it gets its own line with nothing after it**, and any
`| head`, `| tail`, `| grep` or `| jq` you want goes in a SEPARATE statement over
the captured output. If you find yourself typing `$?` on a line that also
contains a pipe, you have already lost the status.

**A backgrounded command hands the same lie to the HARNESS, which then reports it
to you as fact.** Measured 2026-08-29: a watcher launched with
`run_in_background` as `wait-fleet-binary.sh ... | tail -20` returned 1 on a
TIMEOUT, and the completion notification said **"exit code 0"** — the pipeline's
status, which is `tail`'s. Everything the earlier paragraphs teach about reading
`$?` yourself is bypassed here, because you never read it: a message arrives
saying the job succeeded. The rule is therefore stricter for background work than
for foreground: **a backgrounded command whose verdict matters ends in the
command itself, never in a pipe** — redirect to a file and read the file
afterwards. Same run, the L3 gate was launched bare and its 0 was real, which is
the only reason the two could be told apart.

**And a status is not a boolean.** Capturing `$?` correctly is only half of it;
the other half is that non-zero codes mean DIFFERENT things and collapsing them
hides the worst one. Measured 2026-08-29, an hour after the paragraph above was
written: a probe classified a gate's exit as "rejected" on 2 and "accepted" on
anything else, so when the script ABORTED at 1 -- an unbound variable under
`set -u`, because a check had been moved above the line that defines what it
reads -- every case printed "accepted" and the validation looked like it was
letting everything through. The defect was the opposite: nothing ran at all.
Print the number, then classify; a probe that maps a range of codes onto one
word cannot tell "it said no" from "it never got there".

And when you match against a gate's output, **strip the colour escapes in a
separate statement after capturing the status**: the runner prints
`<red>FAIL<reset> level 2`, so no fixed-string match spans it (budgetkit paid for
that one, 2026-08-26).

## A live run needs TWO controls: what it measured, and whether it measured ONE thing

`Running` pods do not say which binary they serve, and a green level 3 attributed
to the wrong code is the most expensive kind. Two separate questions, and a run
needs both answered:

- **Content — "which binary is this?"** Ask the container for the path
  (`command -v`), never hard-code it: a stale path yields "no such file", and a
  `2>/dev/null` turns that into "the marker is absent", which reads like a
  legitimate wait. Use a CONTROL string present in every version alongside the
  freshness marker, so "does not match" and "could not measure" exit differently.
  Measured 2026-09-07: seventeen pods `Running` served a binary built from a DIRTY
  TREE — no commit at all — and only content said so.
- **Time — "was it the same binary all the way through?"** Record the pod names
  BEFORE the run and compare them after. Without this, a rebuild midway gives a
  run that measured two binaries and reports one result: the hardest false green
  to suspect, because everything else looks right. What makes it a control rather
  than an observation is that the answer is known in advance.

**The freshness marker AGES, and that is a false green of its own.** A marker
taken from one commit keeps saying FRESH after the next one lands, because
"fresh" quietly becomes "has THAT commit" instead of "has HEAD". Give the script a
guard that compares the commit which INTRODUCED the marker against the newest
commit touching `.go`, and exit with its own status when they differ — "I cannot
answer" must not look like "no". Find the introducing commit with
`git log -S '<literal>' | tail -1`; `head -1` returns the commit that REMOVED it,
which is the wrong end and has cost time before.

**And when the gate cannot attribute, build the attribution yourself.** A run over
a dirty tree is reported as NOT attributable to any commit — correctly, because
the gate reasons about the TREE. You can still state which BINARY it measured,
which the gate does not know. Do not discard the run; report what it measured and
how you know.

## Reporting a live result: three parts, and the second is what makes the first mean anything

1. **What it exercised** — with the numbers.
2. **What it did NOT exercise**, explicitly, naming this change's paths that the
   run never enters, plus every check the gate itself declares skipped.
3. **What would have broken had we been wrong** — the only question a
   non-regression run actually answers.

Measured 2026-09-07: a level 3 passed with 384 relays billed and served == billed
exact across ten services, and **none of the day's four fixes was exercised** —
they live on panic, shutdown and failed-hostname paths, none of which occurs in a
healthy run. Without part 2 that green reads as "the day is validated". What it
validated is that the happy path did not break, which is worth saying plainly:
one fix had changed the ORDER of a shutdown and another the NAME under which the
miner registers in a consumer group, and either one wrong does not yield 384/384.

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
