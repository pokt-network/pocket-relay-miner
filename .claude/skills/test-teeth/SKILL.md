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

## The discriminating case is rarely the one that motivated the rule

A rule written against a specific failure gets tested against that failure, and
that test cannot tell the rule from a cheaper wrong version of it.

Measured 2026-09-05. A classifier was required to read a gRPC CODE rather than
the error's text, and the reason was a node whose message says "transaction
indexing is disabled". Replacing the code check with a substring match on "not
found" left every test green — because the not-found error's own message
contains that phrase and the indexer's does not, so both implementations answer
identically on every case anyone had thought to write.

The case that separates them is one nobody was thinking about: an error of a
DIFFERENT category whose text happens to contain the phrase — a transport
failure reading "backend not found in pool". The text version calls that
absence; the code version does not. And absence was the verdict that authorised
a retry, so the cheap version was wrong in the expensive direction.

So when a rule says "classify by X and not by Y", the test has to contain a case
where X and Y DISAGREE. The example that motivated the rule almost never is one:
it is where they agree, which is why it looked safe to write the rule about it.

## A value that exists in the type and cannot be reached

No injection finds this one, because there is no defect to inject: there is a
distinction that was stated and not implemented. It is found by reading what the
prose promises and asking whether the code can deliver it.

Measured 2026-09-05, and it had three layers pointing the same wrong way. A type
carried three states, with a comment arguing carefully that the third existed
because folding it into the second "would assert something nobody measured". The
function's `switch` then returned the second from its `default`, so every
unclassified failure — a timeout, a dropped connection — asserted exactly that,
and the third state was reachable only through a case the author's own comment
called something a real node does not do. The comment said A, the code did B.

The third layer is the one to look for, because it is the loudest: the TEST
consecrated B **under a name that said A**. The case was called "anything else is
not evidence either way" and asserted the value meaning "this node cannot serve
it". The author had the right answer, wrote it in the name, and asked for the
other one in the assertion.

The check: for every value in a closed set, name the input that produces it. A
value whose only path is one the code itself describes as impossible is not a
state, it is a comment.

## A red you expected can still be true of something else

The rule about unexpected greens has a twin nobody applies, because a red that
arrives on schedule feels like the end of the check rather than the middle of it.

Measured 2026-09-05: an injection went red exactly as predicted, and reading WHY
it went red — rather than recording the red and moving on — is what surfaced the
unreachable state above. The assertion that failed was not failing for the reason
the injection assumed.

Cheap version: when an injection goes red, read the failure message and confirm
it names the property you were testing. It costs one line of output.

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

**A BROKEN instrument fails loudly; a DISABLED one leaves by the same door as
success.** The injections that come to mind attack the instrument that cannot
run -- a malformed config, a missing file, a dead binary -- and those exit
non-zero, so they are the easy half. The dangerous half is an instrument that is
perfectly well-formed and simply not looking: it prints the same clean zero that
the finished work will print, and exits zero doing it. Measured 2026-09-07: a
reporting target built on `errcheck` with `check-blank: true` was injected with a
broken config (exit 3) and a missing config (exit 3), both correct. With
`check-blank: false` -- a VALID config -- it printed `0 issues` and exited 0,
which is exactly what it will print the day the 304 sites are fixed. The
acceptance criterion for that work was "the target reports zero", so a disabled
flag would have closed it.

**The defence is a CONTROL MARKER, and it has to be executable.** Ask the same
instrument, at the same moment, something whose answer you already know: run the
real config against a file that DOES contain the defect and confirm it is
reported. A criterion written as prose does not run -- the closing session will
be looking at a zero, not at a design document -- so it ships as a script that
exits non-zero when the control fails and says what to re-check. Without a
control, "nothing left to find" and "the flag is off" are the same signal.

**A total that is a well-known round number is a claim about your tooling, not a
measurement.** Same day, same task: a lint run reported "50 issues" twice under
different configurations, and 50 is `max-issues-per-linter`'s default, with
`max-same-issues` capping at 3 underneath it. The real count was 294. The tell
was available before the contradiction: the number was suspiciously round, and
two different configurations produced the identical total. Set the caps to zero
before reading any count, and treat an exactly-default total as unmeasured.

**The general shape behind both: an instrument that hands back LESS than there is,
presented as all there is.** A default issue cap and a glob in a classification do
the same thing. Measured the same day, by both sessions: `cmd/relay/*` written as
one row of a triage read -- to its own author -- as though those files had been
opened, and the row's stated reason turned out false for most of them and
dangerous for two, which were `ReadString` calls guarding destructive-command
confirmations. Writing the scope by PROPERTY instead of by list does not protect
you here, because the property itself came from not looking. The property has to
be derived from having opened the files, never the other way round.

**And a machine trap that comes with it:** a test may READ a gate script
(`tx/metrics_names_test.go` reads `scripts/gates/live.sh` -- verified 2026-09-07;
the path this line named until then, `internal/conventions/metric_coverage_test.go`,
does not exist, so anyone following it found nothing and read that as "no such
guard"),
so editing a `.sh` with a gate run in flight poisons that run exactly the way
editing a `.go` does.

## An injection that did not APPLY gives a green that means the opposite

Measured 2026-09-07, twice in one session. A replacement demanded `count == 1`
and there were TWO identical blocks, so it aborted — and the test ran against the
INTACT tree and said `ok`. That `ok` does not say "my test misses the defect", it
says "there was no defect", and read quickly it sends you to rewrite a test that
was fine, chasing a ghost. The second time Go answered `ok (cached)`: a green from
another tree.

**Verify the injection APPLIED — by counting in the file — BEFORE reading the
result, and run with `-count=1`.** And APPLYING IS NOT ENOUGH: `go build` must be
green before you read anything. A `[build failed]` proves you broke compilation,
not that the test bites; it happened to both sessions the same day, each shortly
after warning the other about it. Applied and compiled are two conditions.

**PUT THE CHECK IN THE DRIVER, NOT IN YOUR HEAD — this paragraph did not stop it
happening again.** Measured 2026-09-09: a teeth script whose injections were
string replacements ran seven of them against a file whose signatures had since
changed. TWO no longer matched anything. Both printed a green that reads as "the
test is weak", and the first was diagnosed that way before the second was even
suspected. The fix is four lines — after each injection, `cmp` the file against a
pristine copy taken at the start and refuse to print a colour when they are
identical — and it found the second dead injection instantly, one that had been
silently dead for an unknown number of runs.

The shape is worth keeping: **a string-replacement injection ROTS with the code
it targets, and it rots silently.** Every refactor of the file under test is a
chance for the driver to stop measuring while still reporting. So the check is
not a step to remember before reading the result; it is a function the driver
calls, and the run is worthless without it.

## Reading a test tells you what it MEANS to cover; only injection tells you what it DOES

A supervisor asserted, from reading the test and its comment, that
`TestUniqueConsumerName_IsStableWithinAProcess` was the only thing holding a
`sync.Once`, and dictated it confidently enough that it was written into a code
comment. Measured: remove the `Once` and the test still passes, because in the
test environment `os.Hostname()` WORKS, so the value is deterministic anyway. The
`Once` only carries weight on the fallback path, where no test went.

It is the family of "the test covers the happy path and the property lives in the
other one", with the twist that here **the happy path PRODUCES the property**, so
nothing looks wrong. And note the transport: **an assertion dictated by whoever is
reviewing gets copied without measuring, because it arrives wrapped in a finding**
— and the finding was correct. It travelled in both directions that day.

## Narrow the injection: a red in a NEIGHBOURING assertion proves something else

To test whether a count pinned a guard, `s.running = false` was removed. Red — but
in the `Eventually` on `IsRunning`, which sits EARLIER in the test's path, not in
the count. Narrowed to `if false` on the guard alone, leaving the bookkeeping
intact, the red landed where it belonged: `expected: 1, actual: 3`. **Before
believing a red, read WHICH assertion caught it.** It is the injector-side sibling
of the accidental witness.

## An injection validates that the test detects the defect; it does not validate the TEST

Three injections passed and none caught that the test itself **copied a
`sync.Once` by value** while saving and restoring it. `go vet -tags test` caught
it. And it was not style: a copied `Once` has its own `done` flag, so the
save-and-restore written to PRESERVE state could run the body twice and break the
very property the test measured — with the test green.

Injections attack PRODUCTION logic. A test can carry defects no injection to
production reveals, and the defence is not more injection: it is that the gate
runs vet and lint OVER TEST CODE. Worth remembering on the day someone says the
gates see nothing — they see nothing of what they do not look at, and exactly what
they do.

## A strong claim in a comment is a promise, and a promise with no test is what you keep removing from OTHER people's comments

A new comment said a log line "is never the misleading half of a pair". Injection:
move the line above the guard it depends on. **Green** — nothing held the promise.
The weight came from context: that commit existed BECAUSE two log lines lied, so
its replacement could lie again one line away. Two exits: write the test, or lower
the prose to describe without promising. **A looser true sentence beats a strong
one nothing holds.**

## Verifying PARTIALLY and presenting it as verified whole

The same shape three times in one day, on three different axes, all by the
reviewer:

- **Reachability**: read a `return err` and asserted a regression, without asking
  whether that return could execute. It could not — the sibling function's only
  return is `nil`.
- **Coverage**: read a test and asserted what it held, without injecting.
- **Exhaustiveness**: read a callee to its FIRST error exit and classified the
  discard as "the callee already reported it". It had two exits and only one
  logged.

So: **"the callee already reported it" is verified by reading the callee to its
LAST error return.** And a corollary that bit the same day: **a reason verified
against a callee that later changes is unverified again, and nothing warns** — the
`Stop()` behind one such reason was rewritten by a later commit in the same
series. It held, but by luck. In an item spanning several commits, re-verify the
reason when the callee is touched.

## A line that entered by the SUPERVISOR's suggestion has nobody to inject against it

"The one who writes does not audit alone" has an axis nobody had written down:
**proposing.** A line suggested by whoever supervises arrives wrapped in their
authority, and then neither side injects against it. The implementer does not --
it is not their code and not their idea. The supervisor cannot ask for it without
that being, in effect, auditing themselves through someone else.

Measured 2026-09-07, on the money path. A floor was added to a resend schedule so
a delayed first attempt would compress into the last allowed block instead of
dropping the second attempt entirely. The reasoning was checked by arithmetic and
is right. Four injections ran against that function that night and **not one
touched the floor**, because none of them was aimed at a line the supervisor had
proposed. Deleting the whole block -- restoring "abandon the attempt" -- left
every test GREEN.

**The rule: whoever proposes runs the injection against their own proposal, and
says so when it comes back green.** Then someone else writes the test, because the
author of a proposal writing its only proof is the same failure one step later.

## An EXACT assertion covers criteria its author was not thinking about

A loose assertion does not even cover its own claim; an exact one covers claims
nobody had in mind when it was written. That asymmetry is why exactness is the
default and not a case-by-case choice.

Measured 2026-09-07, both halves in the same file. The loose half:
`require.LessOrEqual(count, cap)` is satisfied by **zero** attempts, so it could
not tell a cap that held from a resend that never fired -- it did not cover the
thing it was written for. The exact half: a resend calendar asserted as
`[]int64{128}` rather than `>= 128`, chosen purely for readability so nobody
would have to derive the arithmetic. It turned out to be the only thing standing
between a new "compress instead of abandon" rule and the SEPARATE guard that
forbids resending inside the safety margin -- with `>=`, a floor pushing to 129
would have passed. The author was not protecting that guard and had not thought
about it.

So: assert the value, not a bound, wherever a value exists. And when a coverage
table credits a test, check WHICH property of that test does the work -- here it
was the exactness, and a well-meaning later edit relaxing it to `>=` would have
silently uncovered a criterion listed as covered.

## A fixture of ONE cannot tell "once per group" from "once per member"

An assertion can be exact, aimed at the right counter, and still be unable to see
the defect -- not because it is loose, but because the POPULATION is too small for
the difference to exist.

Measured 2026-09-08. A counter was moved deliberately from the per-member loop to
the per-group level, because incrementing per member would have turned ONE fact
into N events and changed what a series shared with four sibling causes means. The
comment explaining that decision was written first and is right. The test seeded
**one** session: per-group increments once and breaks; per-member increments once
and continues. **Both produce 1.** `moved by exactly 1` cannot separate them, and
the injection that swaps the units comes back GREEN.

The fix is one line of the FIXTURE, not of the assertion: seed two members. Then
per-group gives 1 and per-member gives 2, and the same test also covers the break,
which is what its author believed it did.

**The rule: whenever a metric counts aggregates, the fixture needs a group with
more than one member, or the test does not know what it is counting.** Ask it of
any counter whose name is a plural of something that contains things.

## A CHECK CAN BE RIGHT IN EVERY INJECTION AND STILL BREAK THE GATE

The loop above asks one question: does this check go red when it should. There is
a second one, and it is not the same: **what does the check do to the VERDICT when
it does not go red?**

Measured 2026-09-09. A new live-gate cell reporting the in-window resend path was
written with `gate_nothing_measured`, injected against eight fabricated inputs, and
all eight were correct -- Prometheus down, series absent, counter mismatch, each
producing exactly the right outcome. It was still wrong. `gate_nothing_measured`
drops the whole LEVEL, and a healthy localnet loses no transaction, so the resend
path never runs and the cell could never be satisfied. The level came back
`EXIT=2` on a clean run, and the red was the cell's own.

**A gate that is always red stops being read, exactly like one that is always
green.** Both are ways of no longer measuring, and the injection loop cannot see
either: it exercises the check in isolation, where the verdict does not exist.

So, for a check whose HEALTHY value is zero or absent, ask before writing it:

- Can this ever be satisfied on the run the gate actually performs? If the answer
  is "only if something goes wrong", it is not an assertion -- it is a report.
- Which primitive matches? `gate_fail` for a defect. `gate_nothing_measured` for
  an instrument that failed to measure something it SHOULD have measured.
  `gate_pass` with the limit stated in its own message for a capability that is
  wired but was legitimately not exercised. The third one is the one that gets
  skipped, and it is usually the right one.
- And run the gate END TO END after adding it. The unit-level injection proves
  the cell; only the full run proves the level.

**An absent series has two causes that lead opposite ways**, and this is where the
fix came from rather than the mistake: nothing happened (normal), or nobody wired
the counter (a regression that would go silent forever). Prometheus answers the
same for both. They are told apart by asking the BINARY under test -- the metric
name is compiled in whether or not it ever fires -- so a missing NAME fails and a
missing SERIES is a note. It is the same rule as the KeyBuilder corollary and the
batch-limit case: **ask the PRODUCER, not the property**.

The comment that solves this already lived twenty lines further down the same
file, on the settlement breakdown: *"a non-zero would be the finding while a zero
proves nothing"*. It had been read two hours earlier, as history rather than as
an instruction.

## The one-line test for whether this ran

The report names the defect that was injected, quotes the failure showing it named
that defect, and states that the revert left `git diff` empty. "The test passes"
is not a result here — the result is that it FAILED for the right reason first.

## Example

A guard asserting a Prometheus counter carries no `application` label: the
injection is to add `"application"` back to the metric's label set. The test
must go red. Reverting the label must return it to green with an empty diff.

## An assertion cannot see a panic on another goroutine

`require.NotPanics`, and every `recover()`, only sees the goroutine it runs on.
If the code under test SPAWNS the goroutine that dies, the assertion is
decoration: it passes whether or not the defect is present, and the only reason
the suite goes red is that the process itself is killed.

Measured 2026-09-08. A guard was added so a manager wired without a block client
would decline to build its reconciler instead of dereferencing nil, and the test
written for it opened with:

```go
require.NotPanics(t, m.ensureSharedTrackers,
    "a manager with no block client must decline to build the reconciler, not die building it")
```

The dereference happens inside a goroutine that `ensureSharedTrackers` spawns.
Removing the guard DID turn the package red — with `SIGSEGV`, zero tests marked
failed, and the binary gone. The injection therefore looked like a pass for the
assertion, and it was not: the assertion never ran.

Two things follow, and the second is the one that generalises:

- **Assert on STATE, not on panicking**, whenever the failure lives on a
  goroutine you did not start. Here: the reconciler is nil, the store is nil, the
  independent tracker is not. Those are readable from the test's own goroutine.
- **A red is not proof the assertion works.** Read WHICH line the red came from.
  A package that dies mid-run and a package with one failed assertion both print
  FAIL, and only one of them means your test detected anything. This is the same
  family as "a red you expected can still be true of something else", one level
  meaner, because here nothing failed at all.

