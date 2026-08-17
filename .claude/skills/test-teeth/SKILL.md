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
4. **Revert the injection**, byte for byte. Back up the file first and restore
   from the backup; do not retype it.
5. **Re-run. It must be green again**, and `git diff` on the file must be empty.

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

## Example

A guard asserting a Prometheus counter carries no `application` label: the
injection is to add `"application"` back to the metric's label set. The test
must go red. Reverting the label must return it to green with an empty diff.
