# close-session — the steps with a nameable trigger

Read one the moment its trigger fires. These are not optional; they are
CONDITIONAL, and the root file holds what fires on every close.

## A PEER SESSION'S REPORT IS A CLAIM — trigger: another session did work for this one

The close is about to write it into a durable artifact nobody re-derives.
**Verify by command first**, exactly as the machine check refuses to trust your
memory of the machine.

Measured in budgetkit 2026-08-21: a peer reported a core of **622** lines and the
real number was **625**. It corrected itself afterwards — the hand-over would
already have written 622.

Measured here 2026-08-26, in both directions and both times the verification
paid: a peer's claim that this repository's gate ledger "has exactly the bug your
own lib.sh documents" was **half right** (latent, not active — the three call
sites are in the current shell, checked one by one), and a claim that a rule was
*missing* from their close skill was **wrong** — a wider grep found it at
`close-session:794` with different wording. A narrow grep manufactures work for
the other side.

## AN AUTONOMOUS CLOSE — trigger: closing without the human present

It may not fabricate the answers only the human has. The trap question and "what
did you have to repeat" have no substitute: write **"not asked — closed
autonomously"** rather than inventing a plausible answer.

Measured 2026-08-26: an agent asked to close over a state it could not verify
refused, writing that asserting the step had run *"would be inventing a result
with no evidence"*. That is the behaviour, and no wording asked for it.

## RE-READ THE REFUTER RECEIPTS — trigger: an adversarial round ran this session

Against what you ACTUALLY fixed, not against what the round said. A finding
marked confirmed and then fixed differently is a finding still open under a
green label.

## THE CLOSE CANNOT COMMIT — trigger: a commit is blocked or refused

It ships the commits anyway: name every file, its state, and the exact command
the next session runs to land them. A close that ends with uncommitted work and
no instructions has lost the work, not deferred it.

## DO THE DOCS STILL EXPLAIN IT? — trigger: this session changed behaviour a doc describes

`docs/` is written for an operator who was not here. If the change altered what
an operator does or sees, the doc is stale now, and stale beats absent only in
that it looks maintained.

## DID THE PER-ITEM LOOP ACTUALLY RUN? — trigger: this session ran items

Not "did I work carefully" — did each unit go through `item`, with its success
criterion written before, its gate level named, and its queue line. Say which
items did not, and why.

## DUMP THE TRANSCRIPT — trigger: every close that produced findings

The hand-over is judgement; the dump is what was said. When a later session needs
to know what the human actually asked for, the hand-over's summary is the wrong
artifact. `scripts/localonly/DUMP-<date>-<session>-user-turns.md`, and the
hand-over points at it.
