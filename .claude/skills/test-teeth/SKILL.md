---
name: test-teeth
description: Use after writing a test, after a test passes on the first try, or before trusting an existing test as evidence — proves the test actually fails when the defect it claims to catch is present, by injecting that defect and reverting it.
---

# Test teeth

A passing test is not evidence. It is evidence only once you have seen it fail
for the right reason.

Tests that pass no matter what are common and invisible: an assertion on
`len(result) != 0` where the interesting question was `result[0].Address`, a
guard that never executes, a table test whose case was never wired in. They read
as coverage and hold nothing.

## The loop

1. **Name the defect.** Write down, in one sentence, what this test is supposed
   to catch. If you cannot, the test has no claim to verify and that is the
   finding.
2. **Inject exactly that defect** in the production code — not a compile error,
   not a deleted function. The change must be the mistake a person would
   plausibly make.
3. **Run the test. It MUST fail**, and the failure must name the thing you
   broke. A failure for an unrelated reason (a panic three layers away, a
   different test) does not count — the test found chaos, not the defect.
4. **Revert the injection**, byte for byte, from a backup taken **immediately
   before THIS injection, off the state you want back**. Not "once at the start":
   a backup older than your own edits turns the revert into a silent undo of
   them. Measured 2026-09-05: the backup was taken, two fixes were then written,
   and the first revert restored the pre-fix file; the second injection reported
   `substring not found` -- it had nothing left to remove -- and its test went
   red anyway. That red was the tree without the fix, and it looks exactly like
   a red that proves teeth. Do not retype the file, and **never restore with
   `git checkout -- <file>`**: the file usually holds the uncommitted change
   you are testing, so checkout throws that away along with the injection and
   the loss is silent until a gate fails. Measured 2026-08-19: it wiped a whole
   new method mid-session; only `go build` in the level-1 gate caught it. The
   two are the same loss through different doors, and closing only the
   `git checkout` one is why the other stayed open.
5. **Re-run, and it must be GREEN BEFORE THE NEXT INJECTION.** A gate between
   injections, not a step at the end of the loop: in the sequence above, this is
   the check that would have gone red on the revert and named the problem before
   a second injection was ever applied.

   **And a checksum or a `git diff` against the backup cannot do this job.** It
   proves the file MATCHES the backup; it says nothing about whether the backup
   was the right state, because both sides of that comparison come from the same
   place. Two sessions verified the sequence above with matching md5s and neither
   check could see it. Only re-running the test can.

## Before the injection: does the test even reach the fix?

A test that goes red before the fix and green after is not yet proof. It proves
SOMETHING changed behaviour — not that it changed the behaviour you described.

So, with the fix written, read the scenario in your own commit message and
follow it through the function **line by line, down to the fix**. If a `return`
sits between the entry point and your change, the scenario never arrives, and
whatever your test exercised was a different path with the same symptom.

Then look at which INPUT FIELD selects that path (a flag on the message, a
config value, a state field). Test helpers default those to zero, so a helper
that leaves it unset sends every test down the other branch. Set it explicitly
in the test, and assert on it if the test's whole point is that branch.

Measured 2026-08-20: a fix for "a redelivery skips creating the session" was
placed below an `if msg.IsReclaim { ... return nil }` guard. The test used a
helper that leaves `IsReclaim` false, so it drove the non-reclaim path — which
already worked — and passed. Red before, green after, defect alive, and a commit
message asserting the opposite. The injection that catches it: move the fix back
to the wrong side and confirm THAT test goes red. If it stays green, the test is
not pinning the position.

## The assertion that names the defect goes FIRST

An injection can be exactly right and the red still unreadable, because a
different assertion fires before the one that names the thing.

Measured 2026-09-03: restoring the eager backend dial made a test fail on its
close-code assertion with `got read tcp ...: i/o timeout` — true, and one hop
removed from the defect. The assertion that NAMED it, `dials.Load() == 0` right
after the upgrade, sat further down and was never reached. Moving it first made
the red say *"the upgrade alone must not open a connection to the operator's
backend"*.

This is not the same failure as "narrow the injection" below, and the fix is the
opposite end: the injection was already minimal, and it was the TEST's ordering
that had to change. So when a red is technically correct but reads as a symptom,
ask which assertion states the claim, and put that one where nothing can fire
before it.

## What a red tells you

- **Failed with a message naming the defect** — the test bites. Done.
- **Passed with the defect present** — the test is decoration. Fix the test, or
  delete it; a test that cannot fail costs runtime and buys false confidence.
  **But first make sure the injection restored the defect you NAMED**, because a
  green here has two causes and they lead opposite ways. Measured 2026-09-03: a
  cache fix stored a fingerprint only when the directory had not changed, and the
  injection disabled that condition with `if false && (...)`. The test stayed
  green and read as decoration. It was not: with the condition off, the code fell
  through to storing the RE-READ fingerprint — a DIFFERENT repair, which happens
  to close the same case. The original defect lived on the other line (storing
  the fingerprint taken BEFORE the load), and injecting THAT went red at once.
  Mutilating a condition gives you whatever the fallthrough does; it is not the
  same as putting the old code back. When a defect was removed by a commit, the
  cheap check is `git show <sha>` — inject what the minus lines said.

  The unexpected green is worth reading rather than dismissing: it says the test
  cannot tell your fix from that other one. If the other one was considered and
  REJECTED — as it was there, because re-reading moves the failure onto a commoner
  case — then the suite is missing the test that pins the choice, and the green
  just told you which one to write.
- **Failed for an unrelated reason** — narrow the injection. You broke more than
  the one thing.
- **Passed, and the injection WAS the named defect** — then the third cause is
  that the test never REACHED the window. Measured 2026-09-03: a test for a
  backend connection leaked by a close that races a dial closed the bridge
  immediately after writing the frame, so the close usually won and the dial
  never started; the test read green with the guard removed. Synchronising on
  the dependency's own state — the backend handler signalling that it had the
  request, and the test deciding when it answers — made the same injection go
  red at once. The tell is that the assertions never fire rather than firing
  and passing: if the code under test would have had to run for the assertion
  to mean anything, prove it ran. A timing window closed by a duration is a
  window you are guessing at; close it with a channel the test controls.
- **Passed, the injection was the named defect, and the test DID reach it** —
  then the code is fine and something you WROTE about it is not. Measured
  2026-09-05: a range check on an index parsed out of a server's text was
  documented as protecting against an out-of-range value; injecting `if true`
  stayed green, because the loop COMPARES against the index rather than indexing
  with it, so out-of-range was already harmless. The check was not useless — it
  makes a nonsense index audible instead of silently settling a whole batch as
  errors — but the comment claimed a different job than the one it did.

  This one is worth naming separately because no gate can reach it: the code
  compiles, runs, and behaves identically. The claim is a property of the
  EXPLANATION, not of the program, and the damage is deferred — the next reader
  deletes the guard believing it redundant, or keeps it believing they are
  protected from something they are not. The repair is both halves: fix the
  sentence, and add the assertion for what the guard actually buys. Fixing only
  the sentence leaves the guard with no owner.

  It is also why injecting against something that "obviously" holds is worth the
  minute it costs. **An unexpected green is a question, not a result.**
- **Printed the failure and still exited 0** — the harness around the test is
  broken, and the test itself may be fine. Read the exit status, never the
  output: a red you can see and the runner cannot is worth nothing, because the
  gate reads the status.

Measured 2026-08-29, and it was self-inflicted in the minute before: new cases
were appended to `scripts/gates/lib_test.sh` with `cat >>`, which put them
**after the block that tallies failures and exits 1**. The file printed
`lib_test: all cases pass`, then printed two `FAIL` lines, then returned 0. The
injection was caught only because this skill's loop reads `$?` rather than the
text. Appending to a script that ends in its own verdict puts your code past the
verdict — the same family as the pipe that reports `tail`'s status. **Before
trusting a case you added to an existing test file, look at where the file
decides.**

## Rules

- **Never leave the injection in.** Verify with `git diff` that the tree is
  clean before moving on, and never commit while an injection is live.
- **Inject in production code, not in the test.** Weakening the assertion proves
  the assertion runs, which was never in doubt.
- **One defect at a time.** Two injections and you cannot tell which one the red
  belongs to.
- **Scope the edit to the SYMBOL, not the file, and refuse to guess.** Cut the
  segment between the function's `func` and its close, assert it contains
  EXACTLY ONE occurrence of what you are replacing, and edit inside that. A
  common token — `continue`, `return nil`, `break`, `err != nil` — identifies
  nothing in a two-thousand-line file: measured 2026-09-05, a `continue` meant
  for line 471 landed on line 307 and produced a green that was not about the
  code under test at all. An injection that could match twice must abort rather
  than pick for you.
- **Prefer MODIFYING to DELETING.** Removing a line orphans identifiers, and the
  compiler then objects to something that is not the defect: "does not compile"
  is not a red, so the attempt buys nothing and reads like evidence. Measured
  twice in one session — deleting a sort left its import unused, deleting an
  assertion left its variable and helper unused. Inverting a comparison,
  swapping `Index` for `LastIndex`, forcing a condition to `true`: each exercises
  the same defect and still builds.
- **This applies to guard tests especially** — cardinality guards, invariant
  checks, "must not contain X" assertions. They are written precisely because
  the failure is rare, which means nobody has ever seen them go red.
- **A "not compiled" is not a red — prove the build before reading the exit
  code.** The rule above says to prefer modifying; this says how to be sure you
  did. Run the build as a separate step and treat its failure as "injection
  malformed, retry", never as the test failing. Measured 2026-09-05, and by the
  session that had just written the rule: an injection assembled with a bad
  escape put a stray backslash in the file, the runner reported a failure, and
  it was read as a red until the build was checked. A harness that runs
  injections must gate on the compile, because a malformed edit and a caught
  defect produce the same non-zero exit.

## The assertion is right and it is pointed at the wrong collection

Two shapes, one tell. Both were measured on 2026-09-05, on the same test, and in
both the assertion was CORRECT — the `require` message stated the property
accurately — and it was applied where the defect it names cannot appear.

- **An assertion inside a loop is only as good as what the loop iterates.** A
  test grew a pool from two members to five and asserted that indices stay
  stable, inside `for i, old := range before` — the two that already existed.
  The injection renumbered only the members being ADDED, so the assertion ran,
  passed, and was never near the defect. The fix was one word: iterate the
  collection AFTER the change, not the one from before it.
- **A non-membership assertion over an empty set passes for any answer.** The
  same test then checked that newly added members are not in the healthy set —
  and nothing had been probed yet, so the healthy set was empty and the check
  held for every possible index. The fix was to establish the premise: mark the
  pre-existing members healthy BEFORE growing, so "exactly the old ones are
  healthy" can fail.

The check is mechanical and needs no injection to raise the suspicion: **for
every assertion, name the collection it interrogates and ask whether the defect
you are worried about can appear IN THAT collection.** A loop over the subset
that predates the change, a set that is empty at that point, a filter applied
before the mutation — all three answer no, and all three look like coverage.

Distinguish this from the section below on counting: there the assertion is the
wrong KIND, here it is the right kind aimed at the wrong DATA. The first is
caught by reading what the assertion says; this one only by reading what it says
it ABOUT.

## The criterion says WHICH and the assertion says HOW MANY

Writing the standard down does not apply it, and the prose that states it reads
afterwards as evidence that it was followed.

Measured 2026-09-05: a design document said, of its own six success criteria,
"none can be satisfied by counting occurrences — all of them ask WHICH". One of
those criteria was then implemented as ten calls each asserting `NotNil`. An
injection that pinned the round-robin fallback to member zero left it green: ten
calls, ten non-nil results, and no idea which connection answered. The defect
was real — during recovery every call takes that fallback, so pinning sends
every claim to one connection that may be the one still down.

**The check, applied by reading and without running anything: for each criterion
phrased as "which", look at whether its assertion NAMES AN IDENTITY** — an
index, an address, an error sentinel, a specific element. `NotNil`, `len(x) > 0`,
`err != nil` and `Empty` are quantities and negations; all four pass without
knowing which case occurred. A criterion whose assertion counts is not
implemented, it is described.

This is the mirror of the overclaiming comment: there the prose said more than
the code did, here the prose says the right thing and the code does not follow
it. Both are true sentences sitting next to something that does not match them,
and neither is reachable by any gate.

## Comparing against the neighbour instead of against the property

When two code paths handle the same case differently, the more complete one reads
as the correct one — and it can be violating the property just as surely.

Measured 2026-09-05, twice in two days by the same pair. Enumerating the paths
that drop work without recording it, one branch was held up as the CONTRAST for a
worse one: it logged and it cleaned up, where its sibling did neither. Checked
against the property — "nothing is removed without emitting its verdict" — it
fails too, only audibly. It had been left out of the count because it was being
measured against its neighbour. The same shape, a day earlier: a cardinality
budget justified by comparing against zero rather than against what the process
already emits, which was three orders of magnitude larger.

The check: when you catch yourself saying one path is fine BECAUSE it does more
than another, you have changed the denominator. State the property and evaluate
each path against it alone. This is also the argument for scoping work by a
property rather than a list of sites — a list makes the comparison against
neighbours feel like the work.

## A discarded error is not a defect until you follow it

`_ =` on a call that returns `error` looks like a swallow every time, and reading
it is enough to SUSPECT and never enough to assert. Three levels, measured on one
symptom on 2026-09-05, gave three different answers:

1. **Can the callee return non-nil at all?** Two `_ =` on a recording function
   turned out to discard an error that is statically nil for those arguments —
   the only error path was in a branch those calls never take. Not a defect. What
   remains is that the safety depends on the implementations while the interface
   promises an `error`, which is a defence worth writing, not a loss.
2. **If it can, is it reachable from here?** A second `_ =` did discard a real
   one: the callee's first statement queries a store, and that error propagates.
3. **Does the error even reach the discard?** For the sibling call it did not —
   the callee opened with a read whose error it turned into `return nil` under a
   comment saying "nothing to do". The verdict was lost one level BELOW the
   `_ =`, so a correct analysis of the discard would have cleared a function that
   loses data. And the conflation there is its own bug: absent, unreadable and
   corrupt all arrived as one opaque error, so "I found nothing" and "I could not
   look" produced the same answer.

The trap in the middle of this: a function's FIRST error path is not the
function. Both reviewers concluded from one branch and generalised, one of them
with the contradicting line in output he had already read.

## Two reviewers can confirm each other's error

The same session had each of two sessions verifying the other's claims, which
catches a great deal — and it fails in exactly one shape: when both read the same
code wrongly in the same direction, cross-checking CONFIRMS the error instead of
breaking it. It took four readers who had not been in the conversation to catch
it.

That is a measured argument for a council that is separate from "they think
differently": they have not inherited the mistake being passed back and forth.
When two people agree about a piece of code they have been discussing, the
agreement is worth less than it looks, and worth least precisely where the
discussion has been longest.

## Merging two criteria into one: enumerate the injections on both sides

A merge that replaces two criteria with one is only free if the survivor keeps
**both injections**. Check it mechanically, because the failure is invisible from
the inside: **list the injections before and after, and if one lost the criterion
that turned it red, the merge cost something.**

Measured 2026-09-05, on the success criteria for `TxRejection`. Two criteria
covered one type's `Error()`:

- one built the value **through the real code path** and pinned the message byte
  for byte — it went red when a construction site populated a field wrongly;
- one built it **by literal, bypassing the constructor** — it went red when the
  implementation *stored* the message instead of deriving it.

They were merged into the literal-built one, and that read as strictly-more:
against a stored string it is the stronger test, and a stored string was the
defect under discussion. But the literal pins the format **given** the fields,
and nothing was left pinning that the real construction sites **populate** those
fields. The design document named the escaping case in its own prose — two
nearly-identical strings in the struct, and choosing the wrong one is invisible
to `Contains` — and the same revision deleted the only criterion that saw it.

**Why it is invisible**: the merged criterion really is stronger **on the axis
you were looking at**. That is the same shape this skill fights one level down —
an assertion that does not distinguish what it claims to distinguish — raised
from the content of one test to the structure of a set of them. So the remedy
also rises: not "look harder", but a procedure. Two injections that survive the
merge means one criterion; two injections where **neither turns the other red**
means two criteria, and merging them is not a simplification, it is a deletion
with a simplification's face.

## A whitelist of error cases needs a closed-set test

A hand-enumerated set of cases -- which failures are permanent, which
directories are skipped, which panics are allowed -- has a failure mode a table
of examples cannot catch: **forgetting a member is silent.**

`isPermanentKeyFailure` (`keys/keyring_provider.go`) cost four commits to that
shape, twice with the same symptom: a permanent failure classified as transient
leaves the reload abandoned forever while a pulled key keeps signing. And
`TestIsPermanentKeyFailure` could not have caught either -- it is an OPEN table
of the cases somebody already thought of, green before and after the fix that
added the case it was missing.

The test with teeth enumerates the error EXITS of the function and fails when a
new one appears **undecided** -- not when a new one is not permanent. That
distinction is load-bearing: of the six exits, two are deliberately transient
with the reason written down, because a `.info` file caught mid-rewrite would
otherwise turn a half-written file into a supplier removal. So the assertion is
"every exit has a written decision", never "every exit is permanent".

**And the enumeration must come from a source the function does not control.**
This is where the obvious implementation is a TAUTOLOGY: iterate one shared list
in both the function and the test, and the two sides cannot disagree -- the guard
then holds for any list at all, including a wrong one. Measured 2026-08-26: an
agent handed only the paragraph above proposed exactly that, having diagnosed the
problem correctly first. The mechanism that discriminates reads the code rather
than a declaration -- the AST of the function's own `return` statements, or the
errors the package exports -- and compares THAT against the written decisions.

The same trap in its general form, imported from budgetkit (2026-08-22): a
mutation went red, was reverted by hand, the step read as working, and both sides
of its comparison came out of the same parsed file. It proved the check RUNS. It
never proved the check DISCRIMINATES, and those are different claims. Before
trusting any guard, ask where each side of its comparison comes from; if the
answer is the same place, it cannot fail.

## A GATE is a test, and it is the one nobody injects into

The skill gets pointed at `_test.go` and stops there. Measured 2026-08-27: the
Go tests of a change were injected and proven to bite, and the 237 lines of shell
that the same change added to `scripts/gates/live.sh` were read carefully and
never injected into. Two reviews then found, in that shell, a delta that came out
zero whenever the before-snapshot was empty -- which is the NORMAL shape for a
CounterVec that has not fired -- so the first loss a run ever saw would have
printed "series present and unchanged over the run". A green money gate that
cannot go red.

The reading pass is not a substitute and the difference is nameable: reading
answers "what does this check?", injection answers "can it fail?". Only the second
is evidence. The angles named before that reading pass were removed behaviour,
callers, double-counted metrics, language pitfalls and efficiency -- five angles,
and not one of them was "the arithmetic of the measurement".

**How, when the test is a shell gate:** EXTRACT the real block with `sed` and run
it against fabricated inputs with the `gate_*` functions stubbed to record which
one was called. Never copy the block into the harness -- a copy drifts from the
original and then the harness proves something that is no longer there. Then run
the SAME harness against the pre-fix version of the gate (`git show <sha>:<path>`)
and watch it go red: a harness that only passes on the fixed gate has not shown
it would have caught anything.

Working example, written that day:
`scripts/localonly/_state/teeth-live-gate.sh` -- six cases, and it reports which
defect each one catches and, in its header, which defect it does NOT cover.

**The baseline ROTS, and it rots the moment you succeed.** A harness that
compares against `HEAD` is comparing the fix against itself as soon as the fix is
committed — measured 2026-08-29: `pre=1 post=1`, printed as "no teeth" about a
gate that was fine. Anchor it to the commit where the DEFECT IS PRESENT, found by
its own text rather than by a hand-written SHA. And note which end of that search
you want: `git log -S '<string>'` lists the commit that REMOVED the string and
the one that ADDED it, newest first, so `head -1` hands you the removal — a
baseline with the defect already gone. `tail -1` is the one that has it. Both
mistakes happened in the same session, hours apart. Re-run the harness AFTER
committing; that is the only way the rot shows.

**When the INJECTION comes from the environment, it expires.** A harness whose
defect condition is a live state — a deleted pod inside a metrics window, a
stopped service, a full disk — proves nothing once that state is gone, and it
must say so DIFFERENTLY from a failure. Measured the same day: the window harness
printed "no teeth" three hours after the pod it needed had aged out of the query
window, which reads as "the gate got worse". It now exits with its own status and
says what to re-inject. "I had no injection" and "the guard has no teeth" must
not produce the same signal — the same rule the gates themselves run on.

**A guard must certify the thing it DEPENDS ON, not a proxy for it.** This is the
shape that survives a teeth pass, because the guard does fire — on the wrong
question. Measured 2026-08-29: a sentinel was added so an empty result could be
told apart from a failed read, and it was emitted on the HTTP call exiting zero.
The dependency was not the status, it was the PARSE: a 200 carrying a proxy error
page, or an empty body, exits zero and yields no rows, so the sentinel certified
a baseline that had measured nothing, and the false pass it was written to close
was reproduced with a stubbed transport, number for number. Ask what the next
line actually relies on, and certify that. The gap is invisible in a happy-path
test, so the injection has to be the ugly success: the 200 that is not an answer.

**And a machine trap that comes with it:** a test may READ a gate script
(`internal/conventions/metric_coverage_test.go` reads `scripts/gates/live.sh`),
so editing a `.sh` with a gate run in flight poisons that run exactly the way
editing a `.go` does.

## The one-line test for whether this ran

The report names the defect that was injected, quotes the failure showing it named
that defect, and states that the revert left `git diff` empty. "The test passes"
is not a result here — the result is that it FAILED for the right reason first.

## Example

A guard asserting a Prometheus counter carries no `application` label: the
injection is to add `"application"` back to the metric's label set. The test
must go red. Reverting the label must return it to green with an empty diff.
