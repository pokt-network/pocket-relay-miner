---
name: verify
description: Use when about to claim work is done, before committing or pushing, or when asked whether something broke — runs this repository's quality gates at the right level and reports what was verified and what was not.
---

# Verify

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

**Level 3 is not optional for the money path.** A change to relay, claim, proof,
settlement or metering is not verified by unit tests. If `live.sh` does not
exist yet, `all.sh` prints it as NOT RUN — say so in your report rather than
letting level 2's green stand in for it.

## Read the result

Each gate's last line is its verdict; `all.sh` prints a summary. Three outcomes,
and they are not the same:

- **PASS, every gate ran** — the only one that supports "verified".
- **PASS with gates not run** — a tool was missing or a gate does not exist.
  Report it as "X passed, Y not run", never as green. "I found nothing" and "I
  did not look" must not produce the same signal.
- **FAIL** — go to the `gate-triage` skill. Do not re-run hoping for green.

## Reporting

State what you ran and what it proved. Not "tests pass" but "level 2 passed:
suite, race and coverage, whole tree". If you ran `PKG=`, say which package. If
a gate was skipped, name it and say the coverage is incomplete.

Never claim a gate's result you did not observe. Run it and read the output.

## Related

- `test-teeth` — before trusting a passing test, prove it can fail.
- `gate-triage` — when a gate goes red.
