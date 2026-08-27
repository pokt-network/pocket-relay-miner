---
name: gate-triage
description: Use when a quality gate, test or CI job goes red — classifies the failure as caused by your change, pre-existing, or environmental, and says what to do about each. Use before re-running anything.
---

# Gate triage

A red gate is information. The job is to find out what it is telling you, and
the first rule is that **you do not re-run to make it go away**. A second run
that comes back green has not fixed anything; it has hidden the schedule,
timing or state that produced the first result.

## Classify by the ASSERTION, never by the exit code

An exit code of 1 says something failed. It does not say what. Read the assertion
that broke — the message, expected-versus-got, the line — and attribute from
that. Measured 2026-08-26: a shellcheck warning on `live.sh` looked like it
belonged to the change until the same check was run against the committed
version, where it was already there; the exit status said nothing either way.

## Classify before acting

Ask in this order.

**1. Did my change cause it?**

Check out the base commit, run the same gate, compare. That is the only answer
that settles it — reading the diff and reasoning about it is a guess.

```
git stash && scripts/gates/<gate>.sh   # or: git worktree add … <base>
```

If it is red at base too, it is pre-existing. Go to 3.

**2. Is it a data race?**

`WARNING: DATA RACE` is never a flake. The detector reports a race it observed;
not observing it next run means the goroutine schedule differed, not that the
race is gone. Fix it. Re-running until green is how a race reaches production.

**3. Is it "pre-existing"?**

In this repository that is not an excuse — CLAUDE.md says so explicitly. If it
fails now, either your change broke it or it was already broken; either way it
gets diagnosed. What changes is the *handling*, not whether you look:

- broken by you → fix before continuing;
- already broken → say so out loud, with the evidence that it fails at base, and
  ask whether to fix it here or track it. Do not silently proceed.

**4. Is it environmental?**

A missing tool, no cluster, no network. These arrive as SKIP, not PASS — if a
gate skipped, the level is not covered and the report must say so. An
environmental failure is still a finding: it means this machine cannot verify
what it claimed to verify.

## The environment checks, in the order that pays

Run these BEFORE reading a line of the diff.

1. **The test Redis on 6399 dies on its own.** `connection refused` in a suite
   that passed an hour ago is that: `eval "$(scripts/gates/redis.sh up)"` and
   retry. Not a regression.
2. **Another product is on this machine.** `default` is this repository,
   `budgetkit-dev` is not, and its containers and `go test` runs compete for the
   same CPU. Read `cwd` and the parent of any process before attributing it —
   twice on 2026-08-26 a process that looked like ours belonged to the other repo.
3. **A live stack beside the suite.** Tilt building images starves the test
   containers and surfaces as a timeout inside a money test.
4. **The port that came up IPv6-only.** After `tilt up`, 8180 can be `[::1]`
   only; a tool that assumes IPv4 fails against a healthy stack.

## And the scope of your own command is a HYPOTHESIS

Before concluding that something is broken — especially somebody else's — re-run
with the scope spelled out: `-n`, `-A`, the absolute path, the explicit context.
Measured 2026-08-26: `kubectl get pods` with no `-n` answered about the wrong
namespace and read as "a peer deleted our stack" while eleven pods were Running
the whole time. **A false negative sends you looking; a false positive ships.**

## A red that OSCILLATES is an item, never a label

A test that oscillates is a defect. Ask WHICH before touching anything: fix the
FEATURE when it is a real race, redesign the TEST when it depends on an arrival
order it does not control. "A known flake" is neither.

**And a green run is not proof.** Make the failure DETERMINISTIC by injecting
what it depends on, then show the injection turns the red green again. Budgetkit
measured it: five package runs reproduced a live flake ZERO times while a 300 ms
delivery delay failed it 100%, and the same injection enumerated ELEVEN more
tests carrying the identical defect. `test-audit` is that instrument.

## Failure shapes seen in this repository

- **`[build failed]` for a package you did not touch** — something added a `.go`
  file where the toolchain picks it up. `scripts/localonly/_rescued/` is prefixed
  with `_` for exactly this reason: Go ignores `_`- and `.`-prefixed
  directories. A stray `.go` outside them compiles into the build.
- **Green under `make test`, red under coverage** — instrumentation widens
  timing and surfaces real flakes. Treat the coverage red as the true result;
  it is what CI runs.
- **A test passing that should not** — the test may have no teeth. Use the
  `test-teeth` skill rather than trusting it.

## The one-line test for whether this ran

Say WHICH of the three classes the red was -- mine, pre-existing, environment --
and **the command that decided it**, not the reasoning that suggested it. Then the
assertion that broke, quoted rather than paraphrased, and what you propose. Quote the actual error, not a paraphrase. If you could not
determine the cause, say that — an unexplained red left as "probably flaky" is
the most expensive thing on this list.
