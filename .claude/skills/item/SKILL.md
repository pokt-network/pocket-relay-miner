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

### Choosing the approach — THE COUNCIL RUNS FIRST, on every item

**Invoke the council before the first edit of ANY issue or feature.** Not when the
space looks wide, not when the fix looks hard, not only for product code — every
item. Jorge, 2026-08-29, after a session skipped it: *"claramente te instrui que
para cualquier fix usaras el council, porque? simple, para que la solucion sea la
mejor, no la primera que se te ocurrio, para que te hagan dudar, re-pensar el
caso."* And the reasoning behind it, his words: *"2 piensan mejor que 1 y 3 mejor
que 2; aveces es ruido, pero del ruido filtrado, salen otros puntos importantes
que ayudan a considerar la mejor solucion."*

**The council is not the review.** A review reads code you already wrote; the
council reads the SPACE OF APPROACHES before you pick one. They do not substitute
for each other, and running three rounds of review afterwards does not pay back a
council that never ran.

This step exists because of this repository's most expensive measured failure: the
signing key classification took FOUR commits — `efd2fb8` → `fb46b85` → `47116f8`
→ `2ce1d30` — and the last one reintroduced the exact failure mode the previous
one had just fixed, leaving a reload abandoned forever while a pulled key kept
signing. **It recurred on 2026-08-29, worse**, in `scripts/gates/live.sh`: five
defensible fixes existed for one defect, the session chose one from a single
paragraph of its own reasoning — reading the old "invoke it when the space is
wide" bullet as permission to skip — and it cost ELEVEN commits on one file, a
level-3 run in RED, and a correction that reintroduced what the previous one had
just fixed. That bullet is gone; this paragraph replaced it.

**And the trap that made skipping feel safe:** a measurement that proves the
PROBLEM does not authorise the FIX. That session had measured, correctly, that an
instant Prometheus query loses a dead pod's series — and took the confidence from
that measurement into choosing a fix it had not tested against three of the four
consumers that read the value.

- **Size it before spending it**, always: say roughly how many members and how
  many tokens, in the same message that proposes it. A fan-out nobody sized is
  the failure Jorge has to catch.
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

### Before the commit — get the WRITING evaluated, not just the approach

The council reads the space of approaches BEFORE you pick one. Nothing in this
skill used to read what you actually WROTE before it landed, and that gap has a
measured shape: **the code was right and the prose around it overclaimed, three
times in one session** (2026-08-31, `keys/keyring_provider.go`). Each round of
review found that the comment written to explain the PREVIOUS fix asserted more
than had been measured:

- "two causes that are byte-identical from here" — they were only
  indistinguishable because the code did not look; the dependency offered a
  discriminator that was never tried.
- a commit message reading "releasing every supplier must never be silent",
  written in the commit that left the MAXIMAL form of that event silent. Measured
  after the fact: the counter did not move.
- a clamp comment reasoning only about removals, when an addition in the same
  window produces the same reading.

Jorge, that day: *"eso te pasa por no revisarte lo que vas a ir a escribir, te vas
a escribir o hacer fix sin consultar alguien que te evalúe a vos."* The council
evaluates the PLAN. Nothing was evaluating the author.

So, before `git commit`, and on the diff you are about to commit:

1. **Read every claim you wrote as if someone else wrote it.** Comments, commit
   message, doc edits. Mark each sentence that asserts a property of the world:
   "cannot", "never", "must", "identical", "the only", "no readers", "always".
2. **For each one, point at the measurement or mark it.** A command you ran, a
   test that goes red without it, a line of the dependency's source. If you
   cannot, write "not verified" — `CLAUDE.md` already requires this of durable
   artifacts, and a comment and a commit message are both durable. The rule
   existed and did not fire, because nothing made it fire HERE.
3. **Then get it evaluated by something that is not you** — `/code-review` on the
   staged diff, or a subagent asked to attack the claims specifically. Afterwards
   is not the same: a review that runs post-commit turns a claim you could have
   deleted into history someone has to correct.

**And the failure one level below overclaiming: a measurement you ran, read for
its RESULT and not its CONSEQUENCE.** Measured 2026-08-31, same session, and it
cost the HIGH that all three review rounds were about. A probe printed that a
corrupt record under `key_names` comes back as an ERROR rather than being
silently skipped. That was read as "so this branch already catches corruption,
and needs no guard" — correct, and the wrong conclusion. Being caught meant the
provider returned an error, which meant `manager.Reload` abandoned the reload and
kept the previous key set, which meant a key withdrawn afterwards kept signing
forever: the exact defect the session was fixing, still alive in the branch its
own measurement had just visited. The probe was right. The sentence after it was
not.

So for every measurement used as evidence FOR a decision, write the next hop:
not "X returns an error" but "X returns an error, which its caller turns into Y,
which for the operator means Z". The measurement ends at the value; the claim
does not.

The cheap tell that this step is being skipped: the diff explains WHY at length
and the reply says the change is small. Length of justification is not evidence,
and it is where the overclaiming lives.

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

**When you change how DATA IS PRODUCED, enumerate the consumers and walk each one
separately.** Naming the angle is not doing it: measured 2026-08-29, "the
arithmetic of the measurement" WAS on the written angle list, and the change
still shipped validated against one consumer out of three. It was correct for the
consumer that sums per-series deltas and wrong for the one that compares a single
delta to a target and the one that aggregates before differencing — level 3 went
red on a healthy fleet, naming a pod the session had deleted itself. Then the
correction repeated the shape one level down: it created a FOURTH consumer of the
same helper and gave it none of the handling the other three had, eleven minutes
before a commit whose whole subject was "it was the only one reading just the
delta".

So: list the consumers by `file:line` first, then walk each. A producer with N
consumers is N checks, and the count is the cheap part — it is knowing there were
N rather than one that the two rounds above cost.

## The tree — what each finding of a review earns

- **Branch 1 — fix now**: money, data loss, a guard that does not bite, anything
  on the relay / claim / proof / settlement path. Branch 1 does not only say "fix
  now", it says HOW the fix is chosen — that is the council step above.
- **Branch 2 — file it in the queue with its position**: real, not now, and the
  queue line is what makes it real.
- **Branch 3 — say why it is not a finding**, in one line. Silence and "not a
  problem" must not produce the same signal.

**A finding that re-enters a DECIDED topic is not a finding — it is the decision
being re-litigated, and it is branch 3.** Jorge, 2026-08-31, after three rounds
each opened with a HIGH about the same tension: *"así dejamos de tener ya estas
preguntaderas de HIGH, por el mismo topic. En realidad no son findings, es dar
vuelta sobre lo mismo."*

The three rounds that day reported, as three separate HIGHs, three doorways into
one room: a key source that reports a failure makes the manager hold its previous
keys, and holding them is either the protection or the bug depending on which
disk state you walk in with. Each round the session patched the doorway it was
shown and the next round found another. What ended it was not a fourth round; it
was the owner deciding the policy — *an error freezes, it is reported, the
operator repairs it* — and that decision being written where the next reader hits
it: in the operator doc, and in a test that goes red if anyone silences the
reporting.

So, when a round raises something whose ROOT is already decided:

- do not fix it, and do not treat it as new. Name the decision, name where it is
  pinned, and move on. That is branch 3 with an address.
- if it is NOT written down anywhere a reviewer would find it, that is the real
  finding, and the work is to write it down — not to change the code again.
- **the count that matters for stopping is TOPICS, not findings.** A second round
  on the same topic is a signal the topic needs an owner's decision; a third is
  proof of it.

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

The reply names the success criterion, **what the council said and what was done
with it**, the gate level with what it exercised, and the queue line that holds
the item. If any of the four is missing, the item is still open — and "I ran a
review afterwards" does not fill the council slot, because the two answer
different questions.
