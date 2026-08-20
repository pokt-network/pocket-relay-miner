---
name: gate-triage
description: Use when a quality gate, test or CI job goes red — classifies the failure as caused by your change, pre-existing, or environmental, and says what to do about each. Use before re-running anything.
---

# Gate triage

A red gate is information. The job is to find out what it is telling you, and
the first rule is that **you do not re-run to make it go away**. A second run
that comes back green has not fixed anything; it has hidden the schedule,
timing or state that produced the first result.

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

## Reporting a red

Say which gate, what the failure was, whether it reproduces at base, and what
you propose. Quote the actual error, not a paraphrase. If you could not
determine the cause, say that — an unexplained red left as "probably flaky" is
the most expensive thing on this list.
