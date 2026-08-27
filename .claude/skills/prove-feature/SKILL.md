---
name: prove-feature
description: Judge whether a feature is actually PROVEN before it is declared complete — whether the tests and the live matrix exercise it against the acceptance criteria written before the work. Use when a feature's work is finished and green, before any adversarial round and before reporting it done.
---

# prove-feature

**The question this answers, and it is the same question in budgetkit:**

> Do the tests and the live matrix actually EXERCISE the feature being declared
> complete, measured against the acceptance criteria written BEFORE the work?

Not "do the tests pass" — that is `gates`. Not "does this one test bite" — that
is `test-teeth`. This asks whether the thing you are about to call finished is
*observed anywhere*.

**This runs FIRST in a feature's review, before any refuting.** Refuting an
uncovered feature is refuting a description of it.

## Why this exists here, and it is the most expensive kind of green

A key-handling branch shipped a regression that **L3 passed over twice**: the
miner ran with its signing-key hot reload silently OFF. The gate measures relays
served and relays billed, and both were exact — it never touches the reload path.
Nothing was broken in what the gate observes, so nothing went red, twice.

The same shape, counted: **44 of the 45 error and loss metrics are read by no
gate at all** — `upokt_lost_total`, `compute_units_lost_total`,
`relays_lost_total`, `relays_failed_smst_total` among them. Only
`ha_miner_relays_rejected_total` is asserted anywhere. A metric nothing reads is
an observer that was built and never wired.

## The loop

1. **Read the acceptance criteria as written BEFORE the work.** `item` step 1 is
   where they were recorded. If none were written, stop: the feature has nothing
   to be measured against, and that is the finding. A criterion invented now gets
   shaped to what was built.
2. **Map each criterion to something that OBSERVES it** — a cell of the L3
   MATRIX, a script under `scripts/`, or a test whose assertion names it. One
   criterion with no observer is a gap; write it down rather than arguing it is
   implied.
3. **For each observer, ask whether it would go RED with the feature broken.** Do
   not reason about it — that is `test-teeth`, pointed at the live step. A check
   that stays green with the feature reverted is decoration.
4. **Missing steps are ADDED AND RUN. The feature is not done until they are.**
   This is the part that gets skipped, because at this point everything is green
   and adding a step feels like inventing work.
5. **Report the verdict as COVERAGE, never as confidence**: which criteria are
   observed, by what, and which are not. "It all works" is not a verdict.

## Which transport, which mode — naming the cells that did NOT run

For a feature the question is only WHICH live cells land, never *whether*. The
matrix has ten staked services across five transports, and a feature can be
exercised on one and untouched on the rest.

The model, from the signing-key work: L3 passed on `keyring`/`test` and the
hand-over said, in those words, that `keyring`/`file` and `keys_file` were **not
run**. That sentence is the deliverable of this step. An unwritten judgement is
indistinguishable from never having asked.

## The mutation rule, pointed at the live gate

An observer can be present and still prove nothing. The three ways a mutation
lies — the tautology, the one that reddens by crashing, and the check that never
reaches the fix — apply to a live assertion as much as to a unit test. Measured
2026-08-26: `live.sh` asserted served == billed for ten services and reported
**zero units exercised**, because nothing was counting what it had measured. The
assertions were real; the coverage claim was not.

`test-teeth` holds those three; load it when a step's teeth are in doubt rather
than diagnosing from memory.

## What a gap earns

- **The feature is not done.** Add the observer, run it, then declare.
- **If it genuinely cannot be observed live**, say which tests pin the behaviour
  instead, in writing, naming them — and say which transport and which mode were
  not covered.
- **If the criterion turned out to be wrong**, fix the criterion and say that you
  did, never silently after seeing what got built.

## The one-line test for whether this ran

The report lists every acceptance criterion with the observer that exercises it,
and every criterion with none. It is stated as **coverage, never as confidence**:
"it all works" is not a verdict, and a criterion whose observer would not go red
with the feature broken is listed as uncovered, not as covered.

## Related

- `gates` — did the level run, and what did it NOT run.
- `test-teeth` — does this one observer actually bite.
- `test-audit` — a whole AREA suspected of passing for the wrong reason.
- `item` — its step 1 writes the criteria this skill measures against.
