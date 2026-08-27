---
name: close-session
description: Use when wrapping up a work session, writing a hand-over, or about to say the work is left ready — enumerates what the session opened, proves each item reached the queue, and carries the work through to asking for push and PR.
---

# Closing a session

Closing is not writing a summary. A summary describes a state; the queue and the
ask are what execute. Everything below exists because it was skipped.

## Rule zero — never invent a trap, a gate result, a commit count, or a state

Every factual line in a hand-over is either **run and observed in this session**,
**re-verified now with a command**, or **labelled "inferred" / "not verified" /
"assumed"**. There is no fourth option.

Invented filler is worse than an empty section: the empty section says nobody
found one, the filler sends the next session hunting a problem that never
existed. **The absence of a finding is itself a finding, and often the useful
one.**

## The reference files — read one the moment its trigger fires

`references/conditional.md` holds the steps that do NOT fire on every close: a
peer session's report is a claim; an autonomous close and what it may not
fabricate; re-reading refuter receipts; a close that cannot commit; whether the
docs still explain it; whether the per-item loop actually ran; dumping the
transcript. Everything below fires every time.

## 1. The work does not end at the hand-over

It ends at **asking Jorge for push and PR**. Leaving it "ready and flagged in the
hand-over" is not a close — it is the failure, measured: **15 PRs open, none
merged, none with human review**, 13 of them in one stacked chain whose top's
remote sits 6 commits behind the local branch. Each session wrote its finding
into a hand-over nobody actioned.

Committing locally may be delegated. **Push and PR are asked for, never done
alone and never skipped.** And do not ask until BOTH are true on the exact HEAD
being proposed:

- a `/code-review` of **the segment that PR modifies** — its own diff against its
  own base, not the accumulated stack;
- the gates green at the level the change requires (`gates` skill).

Green gates alone are not the bar. A key-handling branch shipped a regression
that L3 passed over twice — the gate measures relays served and billed, and never
touched the path that broke.

## 2. Enumerate what the session opened, and prove each item landed

Not "I noted the findings". List them, then for each one show the line in
`scripts/localonly/QUEUE-deep-cleanup.md` that holds it.

**A finding is not recorded until it is in the queue.** Writing it in a
hand-over, a digest and a session task list feels like three records and is none
— only the queue is read to decide what to do next. Issue #25 has an
evidence file and hand-over paragraphs since 2026-08-19 and is still uncommented
a week later.

## 3. Canonical is a decision, not a date

Declare which hand-over governs, in the queue's header:

```
**Handoff CANÓNICO: `HANDOFF-<date>-<r>.md`**
```

Then run it:

```
scripts/handoff-index.sh
```

Exit 1 = nothing is declared canonical, or the pointer names a missing file. Exit
2 = a hand-over is **newer** than the declared canonical one, which is either a
pointer nobody moved or an old hand-over that really does still govern — say
which, in the queue.

25 hand-overs have accumulated and none was ever overwritten, which is right. The
cost is the inverse: which one governs used to live in a sentence a human had to
keep rewriting. Measured on 2026-08-19, authority was SPLIT across two files —
`HANDOFF-2026-08-19-r3.md` opens with *"Sucede a `HANDOFF-2026-08-19-r1.md`. Ese
archivo sigue siendo válido para lo que **no** se tocó"*, so the newer one
governed its own changes while the pending-work list stayed in the older one.
Nothing on disk said that; a sentence in a memory file did. One pointer, one
file: if part of an older hand-over still governs, move that part.

## 4. Say what you did not finish, in those words

Write the heading "QUÉ NO TERMINÉ" and fill it. Then, for each item, **separate
the blocker from the pretext** — an environment blocker and a decision not to do
it are different, and the reader has to be able to tell.

The model, from 2026-08-26: *"No corrí L3 sobre `5ed2650`, y es la mitad de la
vara. Bloqueante de entorno, no de código: Tilt no está corriendo."* That names
the gap, its size, and why — and it does not dress a choice as an obstacle.

## 5. A green gate belongs to a COMMIT, not to a branch

Say which commit each level passed on. The L3 log on this branch says PASS and is
from `2ce1d30`, **four commits behind** the HEAD it was being read as covering;
the log does not record its own commit, so it had to be corroborated by comparing
its mtime against `git log`. Two levels green on HEAD and the third green
somewhere else reads exactly like three levels green.

Also: `PASS live (2 check(s) skipped)` is not "everything verified". Name the
skips, or the report claims coverage it does not have.

## 6. Ask which rule should have fired and did not

The last question of the close. Not "did I follow the rules" — **which skill was
written for exactly this situation and never got invoked.**

Measured 2026-08-26: `test-teeth` says *"passed with the defect present — the test
is decoration"*. `TestIsPermanentKeyFailure` was precisely its case — an open
table, green before and after the fix it was supposed to pin — and nobody pointed
the skill at it, through four commits of the same defect. A rule that only fires
when someone remembers it is not a rule, and the gap is never which skills exist.
Whatever you find here goes in the queue as a mechanism, not as advice.

## 6b. Ask the human the two questions you cannot answer yourself

Both in their own words, both recorded verbatim, and "nothing this session" is a
valid answer that still gets written down.

1. **The trap** — ask for it, never write one. A trap you invented is Rule zero's
   failure with a different label.
2. **What did you have to repeat?** In these words: *"was there anything you told
   me, that I accepted, and that I still did not do — or that you had to insist on
   before I applied it?"* This is not the trap question: a trap is about the
   machine, this is about the assistant, and it is the more actionable of the two.
   Half the rules both products carry came out of it.

**An autonomous close cannot answer these.** See `references/conditional.md`;
write "not asked — closed autonomously" rather than something plausible.

## 7. Hand the next session its first command

Last step. In the canonical hand-over's §0, write the command the next session
runs before anything else — today that is `scripts/handoff-index.sh`, plus
whatever this session leaves half-done (a gate to re-run, a stash to pick up).

This exists because the trigger has nowhere else to live. A start-of-session
skill has the same defect as any other skill: it fires only when somebody
invokes it, which is the failure in rule 6. And this repository versions
`.claude/skills/` and nothing else — `.gitignore` is `.claude/*` plus
`!.claude/skills/`, enforced by `scripts/check-tracked-files.sh` in CI — so no
hook can be committed to fire it automatically, deliberately. The machine's
memory index can carry the pointer, but it is not in the repository, so a clone
does not get it.

The hand-over IS the surface the next session cannot skip. Put the command
there, and starting stops depending on anyone remembering.

## 6c. Close the PLAN, and carry what is left

`start` step 4b wrote a plan and had it confirmed. Close it item by item: done,
not done, or dropped — and for anything not done, the queue line that now holds
it. A plan that is never closed turns the next session's first question into
"what happened to the last one".

Same pass over the **standing rulings**: this repository keeps them in the queue,
not in a separate ledger, and a ruling's status line rots faster than anything
else because the session that APPLIES it is a later one that never marks it. Check
each open ruling against the TREE, not against the sentence beside it.

## 6d. The skills are LIVING FILES, and the close edits them

Whatever this session learned goes into the skill that EXECUTES it, in this
session, before the hand-over. `item` step 8 states the rule; the close is where
it is verified to have happened. **Name the files you changed.** A lesson that
reaches only the hand-over is read once and describes a state, while a skill runs
every time — and a report that claims a lesson and touched nothing is the same
failure wearing a checklist.

## 7-ante. Write the hand-over, and it has three sections

Named exactly, because the missing one is always the same:

- **Achieved** — with the evidence path for each.
- **NOT achieved** — in those words, blocker separated from decision.
- **Opened** — what this session created that did not exist before, each with its
  queue line.

Then the hand-over is declared canonical in the queue's header, or
`handoff-index.sh` will not see it.

## 7-bis. Review the hand-over. It is a numbered step and it BLOCKS step 8

Open the finished hand-over — read the file, not your memory of writing it — and
extract every sha, test name, path, count and measured number in it. Verify each
one by command. It is the last artifact produced and the only one with no gate
over it, and the next session reads it as fact.

Budgetkit measured the cost of leaving this as a trailing sentence: three
occurrences, and all three times it ran only when Jorge demanded it. A rule that
fires only on demand is not a rule — hence a step, in sequence, where skipping it
is visible.

## 8. Name the next session, reach it, and stop if it does not exist

Numbered step, not a closing remark, and it runs AFTER 1-7 are done: the queue is
written and the hand-over is on disk before this blocks on anything, because if
nobody starts the next session the record still has to survive.

1. **Compute the successor** from this session's own name --
   `<product>-<M>-<D>-<YY>-r<N>` becomes the same date with `N+1`.
2. **Look for it** among the live sessions (`ListAgents`).
3. **Alive** → hand it the posta by name, and say in the report that you did.
   **Not alive** → STOP and ask for it, printing the exact command:

   ```
   claude --name <the computed name>
   ```

**The close is not finished until the next session exists and holds the posta.**
Writing it to a file and calling it delivered is the consolation prize: measured
2026-08-20, a close wrote the full posta and `SendMessage` answered *"No agent
named … is reachable"*; the rule that came out of it -- "write it to a file" --
buys nothing, because a file is read only by someone who already knows to look.
The file stays as the fallback for when the human declines, not as the normal
exit. The point is that Jorge is needed to SUPERVISE and DECIDE, not to be the
transport between two sessions.

**Two cases are asked, never guessed.** A name that does not follow the pattern
has no derivable successor -- measured on this very session, `rm-claude-skills`,
which is why the rule says ask rather than invent. And a close past midnight does
not know whether the next session is today's or tomorrow's.

## 9. Leave the machine as you found it, and kill only what you started

Name what this session started and what it left running. **Never kill by process
name**: this workstation runs two products, and a name-based kill takes the other
session down. Read `cwd` and the parent first — measured twice on 2026-08-26, a
`go test -race` and a `tilt up` that both looked like ours and belonged to
budgetkit.

If you brought the localnet up, say so and say whether you left it up, so the
next session does not spend its first minutes deciding whether the stack is a
leftover or a deliberate state.

## 10. LAST, AND IT IS A GATE: is every pending thing actually tracked?

Walk the session's own reply from the top and, for each thing you said you found,
point at the queue line that holds it. Not "I recorded the findings" — the line.
Anything without one is not deferred, it is lost, and the session is not closed
until it has one.

## The one-line test for whether this ran

The reply names, in this order:

1. the **canonical hand-over**, as `handoff-index.sh` reports it;
2. **what was NOT finished**, in those words, with the blocker separated from the
   decision;
3. the **queue line** holding each thing this session opened;
4. each **gate level with what it exercised**, and what did NOT run;
5. whether **push and PR were asked for**, and the answer.

Any of the five missing means the session is still open, whatever the summary says.

## Related

- `gates` — run them by level, and what a level does and does not prove.
- `gate-triage` — when one goes red.
- `test-teeth` — before any passing test is used as evidence.
