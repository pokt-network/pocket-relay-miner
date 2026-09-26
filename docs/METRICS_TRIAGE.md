# Metrics triage: which series answer which question

This is the list to read after a run, or during an incident, **in this order**.

Two rules before any series:

1. **Read the denominator first.** "500 settled, 0 expired" says nothing until you
   know how many there should have been. Every section below is written as an
   identity (`X must equal Y`), never as a single number.
2. **A counter named after a loss is not evidence of a loss.** Several of ours
   count a failed *attempt*. They are listed as decoys, with what to read instead.
   Measured 2026-09-17: a run that settled 500 of 500 claims with zero expired and
   zero slash reported `upokt_lost_total` = 3034.77 against
   `upokt_claimed_total` = 6742.81 — 45% of the value "lost", none of it real.

All counters are per process and reset on restart, and a restart is normal (HA
failover, rollout, OOM). So a run total is
`sum(last_over_time(<metric>[<run length>]))` evaluated at the END of the run,
never `increase()`: a counter born inside the window is invisible to `increase()`.
Keep the window inside ONE run — a window that reaches into the previous run adds
its numbers silently.

---

## 1. Did we lose claims or proofs?

This is the money question, and only one pair of series answers it.

| Question | Identity |
|---|---|
| Every claim we built was submitted | `claims_created_total` == `claims_submitted_total` |
| Every claim we submitted reached a block | `claim_inclusion_outcome_total{outcome="on_chain_found"}` == `claims_created_total` |
| Every required proof reached a block | `proof_inclusion_outcome_total{outcome="on_chain_found"}` == the number of settled claims whose `proof_requirement_int` is not 0 — **from the chain, not from our counter** |
| The restart resend path ran at all | `proofs_submitted_total` on the RESTARTED process (the counter is per process, so its value after a restart is what that process re-sent), cross-checked with `session_snapshots_resumed_at_startup_total` — **this is the denominator for a restart, read it first** |
| Those resends were not refused | zero `unordered tx ttl exceeds` in the logs, and every session's `proof_inclusion_outcome_total{outcome="on_chain_found"}` |
| The in-window RETRY path, if it ran, was not refused | `proof_rebroadcasts_total{result="error"}` == 0, with `proof_rebroadcasts_total` as its own denominator |

`*_inclusion_outcome` is the only one written **after asking the chain** (the
inclusion reconciler polls `GetClaim`), which is why it is the one that
discriminates. Its other outcomes (`on_chain_missing`, `on_chain_rejected`,
`poll_error`) are the real alarm.

**Two different resend paths, and they are easy to confuse — this table confused
them, twice, on 2026-09-18.** The FIRST is the restart resubmission: a miner that
comes back loads its pending sessions and re-sends their proofs. Its denominator is
`proofs_submitted_total` read on the restarted process, because the counter is per
process. The SECOND is the inclusion reconciler's in-window RETRY, which fires only
when a transaction was CheckTx-accepted and did not land while the window was still
open — that is `proof_rebroadcasts_total`.

Measured 2026-09-18, the two together tell the story: with the wall-clock anchor,
the restart resubmission was refused with `unordered tx ttl exceeds`, the reconciler
kept retrying, and `proof_rebroadcasts_total` reached 238 — so **238 was the SYMPTOM
of the failure, not the denominator of the path**. With the anchor fixed, the same
cut re-sent 216 proofs, all accepted on the first attempt, 250/250 on chain, and
`proof_rebroadcasts_total` never fired at all. So this counter reaching zero is the
good outcome for a restart, and demanding `> 0` (which this table did for one hour)
asks for the symptom as proof that the path ran.

A rebroadcast is still not a defect. Reading `proof_rebroadcasts_total` as "must be
0" while using it as the restart denominator turns a run that never tried into a
pass. Its own refusal lands in `result="error"`, because the `switch` in
`miner/inclusion_reconciler.go` separates only `ErrTxWindowExpired`
(`window_closed`) and `ErrTxAlreadyQueued` (`already_queued`) and a CheckTx refusal
falls through to the default. `success` and `already_queued` are both good outcomes;
`window_closed` is not this defect but it IS work lost, so read it separately.
`triage.sh` prints `NO-DENOM` rather than `ok 0` when no resend was attempted.

The chain's own answer, for a settlement at height H (`localhost:26657` is the
Tilt chain; on beta or mainnet use your node's CometBFT RPC, or the public
Sauron RPC, as `triage.sh` does with `CHAIN_URL`):

```
curl -s "localhost:26657/block_results?height=$H" | jq '
  [(.result.finalize_block_events//[])[]
   | select(.type=="pocket.tokenomics.EventClaimSettled")
   | {pr:([.attributes[]|select(.key=="proof_requirement_int")|.value]|add),
      st:([.attributes[]|select(.key=="claim_proof_status_int")|.value]|add)}]
  | group_by(.pr)'
```

`proof_requirement_int`: 0 = not required, 1 = probabilistic, 2 = threshold.
`claim_proof_status_int`: 0 = pending validation, 1 = validated, 2 = invalid.

### Decoys in this section

- **`sessions_failed_total{reason="proof_tx_error"}`**: this counts a failed
  submission *attempt*. A restarted miner resubmits, and the retry lands, so it
  fires on a run that lost nothing. It also fires on a run that lost everything,
  with the same reason — **it cannot tell the two apart.** The `*_lost_total`
  counters (`relays_`, `compute_units_`, `upokt_`) do not move on a retryable
  proof error: that money waits in the `unresolved` balance until the chain
  answers, and is counted lost only when nothing will retry it. Read
  `*_inclusion_outcome` instead, and read `proof_rebroadcasts_total` for what the
  retries cost. That resubmission only exists when a transaction was actually
  broadcast: the reconciler walks rebroadcast entries, so a session marked
  `proof_tx_error` before any proof was built has none, and nothing retries it.
- **`proof_skipped_total{reason="claimed_root_unreadable"}`** is the opposite
  signal, and it is not a decoy: the miner could not read a session's claimed root
  this block and returned the session to `claimed` to try again while the proof
  window is open. It rising while
  `sessions_failed_total{reason="proof_window_closed"}` stays flat means the
  deferral is doing its job. Both rising together means the window ran out.
  It counts **attempts, not sessions** — one session deferred for four blocks
  increments it four times — so it is not a denominator for anything.
- **`sessions_failed_total{reason="proof_window_closed"}`**: includes sessions
  whose proof was already on chain before the process restarted.
- **`proofs_submitted_total`** does not count a proof that landed on a rebroadcast,
  so `proofs_created_total − proofs_submitted_total` looks like a gap and is not
  one. The gap equals `proof_rebroadcasts_total` when every retry landed.
- **`proof_requirement_required_total` is not a denominator.** It counts the times
  a proof was decided to be required, and that decision is taken more than once per
  session: measured 976 for 500 proofs on the same run. The denominator for proofs
  is the chain's own settlement events.

---

## 1b. Does the money ledger close?

A session leaves the book through exactly one door, so the doors must sum:

Two things about HOW to read it, both measured on 2026-09-18:

- **It is a run-END reading.** Mid-run `claimed` is legitimately larger than the sum
  of the doors, because a session inside its proof window has been claimed and has
  not reached any door yet. Measured during a gate run: 304 claims on chain,
  `claimed` = 384000 uPOKT, every door still 0. That is not the defect this identity
  hunts.
- **Read it in uPOKT, not POKT.** The series are recorded as POKT
  (`Add(cu / 1e6)` in `miner/metrics.go`), so on a small run every term falls below
  1 and an integer reading turns the whole identity into `0 == 0 + 0 + 0` — which
  "closes" and means nothing. `triage.sh` scales by 1e6 for exactly this reason.

| Identity | Series |
|---|---|
| Nothing vanished and nothing was counted twice | `upokt_claimed_total` == `upokt_proved_total` + `upokt_lost_total` + (`upokt_unresolved_opened_total` − `upokt_unresolved_resolved_total`) |

`unresolved` is a session that left through a retryable submission failure and has
not been answered yet. It is a real series rather than an absence on purpose: a
signal whose job is to reveal a gap cannot have a gap of its own. If it stays high,
something is never being resolved, and that is visible instead of silent.

It is a **pair of counters**, not a gauge, and the reason is the restart. A gauge
resets with the process, so a session left unresolved by one replica and resolved
by another reads wrong from either side — the dead replica's series says one is
outstanding forever, or ages out and says none ever was. Two counters summed across
instances give the right outstanding count across a restart, which is the scenario
this system exists to survive.

**Work that never entered the book is counted separately**, in
`upokt_forgone_total{reason}` (and its `relays_` and `compute_units_` siblings), and
it is deliberately OUTSIDE this identity: the ledger measures what entered the
ledger. A claim window that closed before anything was submitted never reached
`claimed`, so counting it as a loss would drive the residual negative. It is still
revenue that was served and not earned, so it is counted where it cannot disappear.
Its reasons are `claim_window_closed`, `claim_tx_error`,
`claim_ejected_unrecoverable`, `on_chain_missing` and `on_chain_rejected`.

Two properties of the unresolved pair worth knowing before reading it:

- It carries `supplier` and `service_id`, so it **cannot be pre-registered at
  zero**. Before the first event the query returns no data at all, which a reader
  must treat as 0 — not as "unknown", and not as a missing metric.
- `phase` only ever takes `proof` today. Opening a balance requires the money to
  already be in the book, and the claim side only enters the book when the chain
  accepts the claim — at which point there is nothing left to be unresolved about.
  The label exists so that the claim side entering the book earlier would need no
  schema change.

Measured 2026-09-17, before this was enforced: claimed 6742.81, proved 4277.10,
lost 3034.77 — a residual of **−569.06**, or 108.4%. A session whose first
submission failed was counted lost at the error and counted proved again when the
retry landed. While that is true, the three series cannot be used together for
anything: there is no subtraction that yields what was earned.

---

## 2. Is Redis the constraint, and did it cost anything?

| Question | Series |
|---|---|
| Are the gates open right now | `ha_transport_store_operable` (1 = open) |
| How long were they closed | `ha_transport_store_closed_seconds_total`, **divided by the number of closed series** — it sums across gates and components |
| How many open/close transitions | `ha_transport_store_transitions_total` |
| How close to the limit | `ha_transport_store_free_bytes`, `ha_miner_redis_memory_usage_ratio`, `ha_miner_redis_used_memory_bytes` vs `ha_miner_redis_max_memory_bytes` |
| What was in Redis when it closed | `ha_miner_store_memory_at_close_bytes` (bucketed by key family) |
| What the relayer refused meanwhile | `ha_relayer_relays_rejected_total` (the 429s: `storage_saturated`) |
| Writes Redis actually refused | `ha_smst_store_errors_total`, and the `denyoom` classification |

**The gate closing is not an incident by itself** — it is the designed response,
and ingestion reopens 512 MiB above the close line, admission 1 GiB above it (at
a `maxmemory` of 8 GiB or more: close below 1 GiB free, reopen at 1.5 GiB and
2 GiB). It becomes an incident
when section 1 shows a gap, or when it never reopens.

### Audit trail

- **`ha_miner_tracking_writes_failed_total{kind,reason}`**: a submission record the
  tracker tried to write and Redis refused (`reason=oom`) or that failed for
  another reason. The record for that claim or proof is missing — not wrong,
  missing. Any figure taken from `ha:tx:track:*` for that window is incomplete.
- **`ha_miner_tracking_outcomes_without_record_total{kind}`**: the inclusion
  reconciler had a CLAIM's on-chain outcome to annotate and found no record to
  put it on. **That outcome is lost for good**, even if storage recovers a minute
  later. Only `kind="claim"` is ever counted; the proof side has no series (see
  "What has NO series at all").
- Historical note: until 2026-09-17 the tracker skipped these writes entirely
  while the storage gates were closed, and a later phase then created a minimal
  record reporting `claim_success: false` for claims that had settled. Measured on
  the run of that day: 250 such records, exactly the claims submitted between
  heights 45 and 54, while the other three sessions of the same run were complete.
  Records written before that date, for a window where the gates closed, read as
  failures and are not.

---

## 3. Is memory the constraint?

| Question | Series |
|---|---|
| Container working set vs its limit | `container_memory_working_set_bytes` — `max() by (pod)` with `container!=""`, never `sum()`: cAdvisor exports the pod series AND the container series, and a dead container's series stays queryable |
| Did the Go runtime hit its soft limit | `go_gc_limiter_last_enabled_gc_cycle` — when this ADVANCES, the GC CPU limiter engaged and the process is allowed to exceed `GOMEMLIMIT` |
| Did the miner hold ingestion | `ha_miner_ingestion_memory_brake_closed`, `ha_miner_ingestion_memory_brake_transitions_total` |
| Did trees leave memory when sessions ended | `ha_miner_smst_trees_unloaded_total` |
| Is a proof waiting for memory | `ha_smst_rebuild_waiting`, `ha_smst_rebuild_oldest_proof_wait_seconds`, `ha_smst_rebuild_wait_seconds` |
| Was the rebuild estimate wrong | `ha_smst_rebuild_heap_growth_over_estimate` |
| How much the relayer's compression shrinks the WAL | `ha_transport_relay_compression_bytes_total{stage="out"}` over `{stage="in"}` is the ratio of the relays it compressed; `ha_transport_relay_compression_total` by `outcome` says how many relays that was (`compressed`) and why the rest travelled raw (`below_threshold`, `probe_incompressible`, `not_smaller`, `over_max`) |

Working set is roughly **2× the live heap** under `GOGC=100` (measured: 1276 MiB
live against 2532 MiB working set). A threshold written against the live heap does
not protect the container.

---

## 4. Did relays reach the tree?

The three counts are **not interchangeable**, and mixing them is how a run gets
declared fine. Served is what the relayer answered; published is what reached the
stream; leaves are what the tree holds; `num_relays` on chain is what we signed.

| Identity | Series |
|---|---|
| Served splits into published, skipped and dropped | `ha_relayer_relays_served_total` == `ha_relayer_relays_published_total` + `ha_relayer_relays_skipped_difficulty_total` + `ha_relayer_relays_dropped_total` |
| Nothing is dropped between stream and tree | `ha_miner_relays_consumed_from_stream_total` == `ha_miner_relays_added_to_smst_total` |
| What the claim actually carried | `ha_miner_claim_leaves_total`, `ha_miner_relays_claimed_total` |
| What the chain credited | `num_relays` in the settlement event, against the leaves |

A relay the miner cannot restore to its original bytes (a compressed field that
does not decode, or a message carrying neither form) is acknowledged and counted in
`ha_miner_relays_rejected_total{reason="relay_bytes_corrupt"}` (or
`reason="relay_bytes_corrupt_redelivered"` when the message was a redelivery, so
match both): it is a defect in the producer, never a retry.

After the first settlement the difficulty rises, so published drops well below
served **by design** — that gap is `relays_skipped_difficulty`, not loss. The
third term, `relays_dropped`, is relays served and never published: read it by
`reason` (`stake_exhausted` is work given away; `publish_failed` is loss).

---

## 5. Transactions

| Question | Series |
|---|---|
| Rejections the chain returned on broadcast | `ha_tx_broadcast_rejections_total{tx_type,codespace,code}` |
| Which rule set the deadline of each signed transaction | `ha_tx_timeout_regime_total{phase,regime}` |
| Are we saturating the broadcast permits | `ha_tx_permit_saturated_total`, `ha_tx_permit_wait_seconds` |

### Decoy in this section

- **`ha_tx_timeout_regime_total` is not the number of broadcasts.** It counts
  transactions SIGNED; `ha_tx_broadcasts_total` counts sends the node accepted,
  fresh or re-injected. A resend that re-injects bytes already signed counts no
  regime, and a signed transaction the node refuses counts no broadcast, so the
  two differ on a healthy run.

---

## What has NO series at all

An absence here is a finding, not an omission to work around. Each of these had to
be diagnosed by hand, and one of them was found only because somebody asked the
right question:

- **A rejection returned by gas SIMULATION.** `ha_tx_broadcast_rejections_total`
  is incremented only from a broadcast response, so a transaction the chain refuses
  during simulation is invisible. Measured 2026-09-17: 238 proofs refused this way,
  found by reading 500 Redis records by hand.
- **Which clock anchored a transaction's timeout.** The code computes the anchor
  source and only logs it.
- **A proof outcome that found no record to annotate.** The claim side is
  counted (`ha_miner_tracking_outcomes_without_record_total{kind="claim"}`, with a
  `Warn`), but a proof's on-chain outcome with no record is a silent no-op: no
  counter and no log, so a proof outcome lost this way is invisible.
- **Time from process start to the first observed block.** Nothing measures it,
  so nobody knows how long a restarted process runs without a chain clock.

---

## Running it

`scripts/observability/triage.sh` evaluates every identity above against
Prometheus for a run window and prints `OK` or `GAP` per line. Run it at the end
of every load run, not only when something looks wrong — the run that produced
every number quoted in this document looked perfect from the outside.
