---
name: test-audit
description: Use when a whole AREA is suspected of passing for the wrong reason — a suite that never goes red, a flake with no red to start from, a mutation that stayed green, code written fast. Audits by INJECTING a dependency's worst behaviour and reading what fails, not by reading the tests.
---

# test-audit

`test-teeth` asks whether ONE test bites. This asks whether an AREA is held at
all, and it answers by **injection, not by reading**: make a dependency behave at
its worst and see what stays green.

Reading a suite tells you what its author thought about. Injection tells you what
the suite would survive.

## When it fires

- a suite suspected of passing without biting;
- a flake with no red to start from;
- a mutation that stayed green;
- an area written fast, or one nobody has broken on purpose.

## The catalogue — injections this repository has already paid for

**Swap miniredis for a real Redis.** This is the one with the receipt: miniredis
answers a blocking `XREADGROUP` immediately, does not age PEL entries and
approximates expiry, and this repository paid for that gap with **a consumer that
could not be shut down while the whole suite stayed green**. `scripts/gates/redis.sh`
exists because of it. Any area touching streams, consumer groups or TTLs is
audited against a real server or it is not audited.

**Make the readiness probe cross the real path.** A wait implemented as
`docker exec <container> ping` proves the server is alive inside its own
namespace and says nothing about the port the client uses — a broken `-p` reads
as ready (measured 2026-08-26, and budgetkit paid the same with `pg_isready`).

**Delay, then cross a session boundary.** Relays that arrive after their tree is
sealed or after the claim window closed cannot be paid. An area that never sees a
boundary crossing has never been asked the only question that matters.

**Starve the block-event channel.** Measured: at roughly 594 suppliers per miner
the channel saturates and state transitions die. Anything reading block events is
audited under a channel that cannot keep up, not under an idle one.

**Take the key source away.** A source that yields nothing, a keyring directory
that disappears, a record that can never yield a private key. Each one has
produced a distinct real defect on this branch.

**Point the metric at nothing.** A counter with no reader is an observer that was
built and never wired — 44 of 45 loss metrics here are in that state.

## The protocol — five steps, and the fifth is the one that proves anything

**1. INJECT.** One behaviour, in the dependency, not in the test. Scope it to the
area you are auditing: a change to shared test scaffolding reddens the tree and
tells you nothing about this area.

**2. LIST THE REDS.** Write down every test that failed, by name, before
interpreting any of them. The list is the measurement.

**3. CLASSIFY EVERY RED — three buckets, and the middle one is legitimate.**
 - *the test caught the injected behaviour* — it has teeth;
 - *the test failed for an unrelated reason* — it broke on the scaffolding, not
   on the behaviour; narrow the injection;
 - *the test was ASSERTING the wrong behaviour* — it pinned what the dependency
   used to do. That is a finding about the test, and it is the interesting one.

**And the tests that stayed GREEN are the output.** A suite where nothing went
red under a worst-case dependency is not a strong suite; it is a suite that does
not reach the dependency.

**4. FIX** what the classification earned, through `item`'s tree — branch 1 now,
branch 2 to the queue with its position, branch 3 with one line saying why it is
not a finding.

**5. RE-VERIFY UNDER THE SAME INJECTION.** Re-run with the injection still live.
A fix judged against the clean tree is a fix judged against the conditions that
hid the defect.

## Removing the injection: restore from a backup, by hand

Back the file up with `cp` before injecting and restore from that backup. **Never
let git discard it for you** — the file usually holds the uncommitted work being
audited, and a checkout of that path throws the work away along with the
injection, silently. Measured 2026-08-19: it wiped a whole new method mid-session
and only `go build` in the level-1 gate caught it. Confirm `git diff` is empty
before moving on, and never commit while an injection is live.

## Cost — scope the injection, and say what you did not audit

An injection wide enough to redden the tree costs a full suite run per iteration.
Audit one area at a time and **name the areas you did not audit**: "I found
nothing" and "I did not look" must not produce the same signal.

## Cross-references — this skill does not restate them

- `test-teeth` — the three ways a mutation lies: the tautology, the one that
  reddens by crashing, the check that never reaches the fix.
- `gate-triage` — a red that may be pre-existing or environmental.
- `prove-feature` — a feature suspected of being unobserved, rather than an area
  suspected of not biting.

## The one-line test for whether this ran

The report names the injection, the tests that went red, the tests that stayed
green under it, and the areas not audited. Without the green list, the audit did
not measure coverage — it measured that something failed.
