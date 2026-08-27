---
name: item
description: Use when starting any unit of work — a finding, a bug, a feature, a queue item — to run it end to end under this repository's process, from sizing it against the tree to closing it in the queue.
---

# Item

One unit of work, start to finish. The steps are the same in both products; what
each step DOES differs where the products differ.

## The three that govern every item

1. **The success criterion is written before the work**, and it is checkable.
   `CLAUDE.md` requires it; "it works" is not one.

   **"This one is mechanical" is not an exemption — it is the case that needs it
   MOST.** Measured 2026-08-27: the criterion was written for a small gate change
   (three checks, all met) and skipped for a branch cascade, on the grounds that
   a rebase is mechanical. The script then took the wrong fork point, replayed
   ZERO commits onto each new base, and **collapsed seven branches and pushed
   them broken**. The criterion that would have caught it on the FIRST link is
   one line — "each branch keeps the N commits it contributed" — and it is
   exactly the `N de N` guard that had to be written afterwards, at the cost of
   a restore. Automation does not make a missing criterion cheaper; it repeats
   the mistake once per item.

   Jorge, same day, on being asked what he had to repeat: *"solo lo de karpathy,
   que sigo teniendo que recordarlo"*. It had already been the finding of
   2026-08-25. A rule that fires only when someone remembers it is not a rule,
   which is why it is step one here and not advice.
2. **A finding is not recorded until it is in `scripts/localonly/QUEUE-deep-cleanup.md`.**
   A hand-over, a digest and a task list feel like three records and are none.
3. **The work ends at asking for push and PR**, not at the hand-over. See
   `close-session`.

## Rule #1 — print the working list, in the reply, before anything else

The steps you are about to run, named. A list you did not print is a list you
will drift from, and the drift is invisible to the person reading the reply.

## Step 1 — size it against the tree, and distrust the plan that describes it

Read the code the item names before believing anything written about it. A plan
is a prediction: measured 2026-08-20, **two of two tests written inside a plan
were broken when finally run** — one asserted a counter that path never
increments, the other compared two different arguments. The plan's "Expected:
FAIL" had never been executed.

If the item came from a review or an issue, re-derive the claim from the code.
Somebody else's finding is a claim until you reproduce it.

## Step 1c — list the assertions that already govern what you are about to change

Before touching it, name the tests, guards and gates that already hold that code,
and say which of them SHOULD go red. Anything you change that no assertion covers
is a gap you are creating knowingly.

Measured 2026-08-26: `gate_exercised` was added to one of `static.sh`'s two
modes. Nothing was asserting the other mode, so nothing failed, and the gate's
contract silently depended on which mode ran.

## Step 2 — do it, under the process the repo already demands

Conventions are tests here (`internal/conventions`), so a violation fails rather
than waits for review. Do not add a knob where a KeyBuilder method belongs, do
not add a bare `go` statement, do not reach for `sync.Map`.

### Choosing the fix for a BRANCH-1 finding — evaluate BEFORE writing code

A branch-1 finding is one where the fix is not obvious and a wrong choice costs
more fixes. **This is where the council is invoked**, and it is the step that
exists because of this repository's most expensive measured failure: the signing
key classification took FOUR commits — `efd2fb8` → `fb46b85` → `47116f8` →
`2ce1d30` — and the last one reintroduced the exact failure mode the previous one
had just fixed, leaving a reload abandoned forever while a pulled key kept
signing.

- **Invoke it when the space is wide**: three or more defensible fixes, each
  touching a different contract. Two options is not wide — choose and move on.
- **It finds STRUCTURAL defects, the kind visible by reading** — a system that
  agrees with its own error. It does not find behavioural ones that only appear
  when you run the thing.
- **Launch members WITHOUT a `name:`.** Naming a subagent makes it an addressable
  peer and its result stops coming back: 4 of 4 mute named, complete unnamed
  (budgetkit, 2026-08-26).
- **The question must carry the STRUCTURE**, with verified `file:line`. Asked
  about the symptom, it deliberates where the defect is not, and that mistake
  costs the whole budget.
- **Propose the cost before spending it** — roughly 800k tokens for four members.
  Jorge's standing rule: a fan-out nobody sized is the failure he has to catch.
- **Its output is a CLAIM, not a finding.** It does not enter the queue without a
  command that reproduces it: the members are all the same model, so agreement
  between them is not corroboration.

## Step 3 — prove the test can fail

Use `test-teeth`. Do not restate it here.

## Step 4 — sweep what the change made stale

Every document that asserted the old state is now wrong, and nobody will notice.
Measured 2026-08-26: the auto-loaded memory index named a hand-over that had
stopped being canonical, in two places, and a queue entry said "NOT DONE" about
something another session had already closed. Grep for the thing you changed,
not for the file you remember.

## Step 5 — the gates, bare

Use `gates`. Level 2 is the floor for "done"; level 3 is not optional for relay,
claim, proof, settlement or metering. Report what did NOT run.

## Step 6 — your own pass over the diff, cheap and without agents

Name the angles in writing BEFORE reading: removed behaviour, cross-file callers,
double-counted metrics, language pitfalls, efficiency. Measured 2026-08-26, that
pass over a 17-commit branch found a real defect the gates could not: a gate
reporting its units in one mode and not the other.

## The tree — what each finding of a review earns

- **Branch 1 — fix now**: money, data loss, a guard that does not bite, anything
  on the relay / claim / proof / settlement path. Branch 1 does not only say "fix
  now", it says HOW the fix is chosen — that is the council step above.
- **Branch 2 — file it in the queue with its position**: real, not now, and the
  queue line is what makes it real.
- **Branch 3 — say why it is not a finding**, in one line. Silence and "not a
  problem" must not produce the same signal.

**When the loop stops** (Jorge, 2026-08-26):

```
run a round, apply the tree
  did this round produce any FIX?   (branch 1, or a reproduced critical)
    YES -> one more round
    NO  -> STOP
hard cap: 3 rounds
```

**"Any fix", not "any finding"** — every round reads code the previous one
changed, so there is always something new to say, and a condition built on
"something new" never converges. The loop exists to catch what the FIXES
introduce, and this repository has that measured end to end: `2ce1d30`
reintroduced the exact failure `47116f8` had just fixed, in the same file, on the
same classification.

**The cap does not exit in silence.** If the third round still produces fixes,
stop anyway and file a card saying **"3 rounds without converging in <area>"**.
That sentence says the area has a structural problem rather than a list of bugs,
and it is worth more than a fourth round.

## Step 7 — close the item in the queue

Write what you did, what you did NOT do, and the evidence path. An item closed
only in the reply is not closed. Issue #25 has an evidence file, hand-over
paragraphs and a queue entry since 2026-08-19, and is still uncommented.

## Step 8 — improve this skill from what the item just taught you

If the item revealed a rule, it goes in the skill that EXECUTES it, in the same
session — not into a hand-over. Measured 2026-08-26: `test-teeth` described what
a closed-set test must contain and not where its enumeration must come from, so
an agent handed that paragraph proposed the tautology. The two halves now live in
one section, because splitting them is what produced the defect.

### When a skill outgrows one file

The rule is FUNCTIONAL, not a line count (budgetkit, 2026-08-26, correcting a
threshold proposed from here): **a lesson stays in the root file when it fires on
EVERY item, and moves to a `references/` file when it has a trigger you can
name.** A line count only tells you to go look — it cannot tell the difference
between a file that is long because it is uncompressed and one that is long
because half of it is conditional.

For a closing skill the same rule reads: the root file holds what the session
closing CANNOT skip; everything with a nameable trigger moves.

## The one-line test for whether this ran

The reply names the success criterion, the gate level with what it exercised, and
the queue line that holds the item. If any of the three is missing, the item is
still open.
