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
4. **Revert the injection**, byte for byte. Back up the file first (`cp`) and
   restore from that backup. Do not retype it, and **never restore with
   `git checkout -- <file>`**: the file usually holds the uncommitted change
   you are testing, so checkout throws that away along with the injection and
   the loss is silent until a gate fails. Measured 2026-08-19: it wiped a whole
   new method mid-session; only `go build` in the level-1 gate caught it.
5. **Re-run. It must be green again**, and `git diff` on the file must be empty.

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

## What a red tells you

- **Failed with a message naming the defect** — the test bites. Done.
- **Passed with the defect present** — the test is decoration. Fix the test, or
  delete it; a test that cannot fail costs runtime and buys false confidence.
- **Failed for an unrelated reason** — narrow the injection. You broke more than
  the one thing.

## Rules

- **Never leave the injection in.** Verify with `git diff` that the tree is
  clean before moving on, and never commit while an injection is live.
- **Inject in production code, not in the test.** Weakening the assertion proves
  the assertion runs, which was never in doubt.
- **One defect at a time.** Two injections and you cannot tell which one the red
  belongs to.
- **This applies to guard tests especially** — cardinality guards, invariant
  checks, "must not contain X" assertions. They are written precisely because
  the failure is rare, which means nobody has ever seen them go red.

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

## The one-line test for whether this ran

The report names the defect that was injected, quotes the failure showing it named
that defect, and states that the revert left `git diff` empty. "The test passes"
is not a result here — the result is that it FAILED for the right reason first.

## Example

A guard asserting a Prometheus counter carries no `application` label: the
injection is to add `"application"` back to the metric's label set. The test
must go red. Reverting the label must return it to green with an empty diff.
