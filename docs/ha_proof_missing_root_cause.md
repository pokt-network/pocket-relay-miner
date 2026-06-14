# HA RelayMiner — `PROOF_MISSING` silent-forfeit root cause

Status: **confirmed in code + reproduced by tests** (2026-06-14).

## Symptom

Mass `PROOF_MISSING` claim expirations at settlement on mainnet, heavily
concentrated on **large multi-key operators** running this HA miner. The claim
lands, but no valid proof is on-chain by proof-window-close, so the whole claim
reward is forfeited (the slash is symbolic; the lost reward is the real cost).
The miner's own logs/metrics report the proofs as **submitted successfully**.

## What it is NOT (ruled out)

Two failure modes common to the stock `poktroll` relayminer do **not** apply
here — this binary already fixed them:

- **Account-sequence collisions across replicas** — N/A. Txs are unordered:
  `tx/tx_client.go:522` `SetUnordered(true)`, sequence forced to 0
  (`tx/tx_client.go:694-698`). Concurrent submissions from multiple replicas on
  the same key do not collide.
- **Tight block-based TTL** — N/A. The tx deadline is a block-time-anchored
  `SetTimeoutTimestamp`, derived from remaining window blocks and clamped to
  `[2m, 10m-10s]` (`tx/tx_client.go:528-559`). It is not a fixed `+N` height
  offset.

## Root cause: SYNC broadcast + no proof inclusion verification

The proof is submitted and its outcome judged **only at CheckTx (mempool)
level**. Nothing verifies block inclusion before the window closes.

1. **SYNC broadcast.** `tx/tx_client.go:607-643` broadcasts with
   `BROADCAST_MODE_SYNC` and returns success on `TxResponse.Code == 0`. Code 0
   means *accepted into the mempool*, not *included in a block*.

2. **Success recorded on mempool acceptance.** On the non-error branch the
   miner stores the proof tx hash, marks the session proved, fires
   `RecordProofSubmitted`, and logs "proofs submitted successfully"
   (`miner/lifecycle_callback.go:1862-1892`).

3. **Retry only on CheckTx error.** The submit loop retries solely when
   `SubmitProofs` returns an error (`miner/lifecycle_callback.go:1811-1860`). A
   proof that is CheckTx-accepted and then evicted / never included produces no
   error → no retry.

4. **No proof inclusion poller.** There is a post-broadcast inclusion poller for
   **claims** (`miner/inclusion_tracker.go` → `GetClaim` →
   `UpdateClaimOnChainOutcome`), but **none for proofs**. `grep -r
   "ScheduleProofCheck\|GetProof\|ProofOnChainOutcome"` returns nothing.

5. **The data model encodes the gap.** `SubmissionTrackingRecord`
   (`miner/submission_tracker.go:20-61`) has `ClaimOnChainOutcome` plus the
   `UpdateClaimOnChainOutcome` reconciler (`:304`). There is **no
   `ProofOnChainOutcome` field and no `UpdateProofOnChainOutcome` method**.
   `ProofSuccess` is explicitly commented "BROADCAST acceptance only — not
   on-chain" (`:49`). A forfeited proof is therefore *unrepresentable* in the
   record — permanently indistinguishable from a settled one.

The post-mortem signal exists only after the fact: `SettlementMonitor`
(`miner/settlement_monitor.go:370 handleClaimExpired`) observes the on-chain
`EventClaimExpired` at settlement — too late to act.

## Why it concentrates on large operators (the amplifier)

Submission **timing spread is disabled** for both claims
(`miner/lifecycle_callback.go:602-606`) and proofs
(`miner/lifecycle_callback.go:1435-1439`): every session's proof is submitted
*immediately at proof-window-open*.

Mainnet shared params (`num_blocks_per_session=60`,
`proof_window_open_offset=1`, `proof_window_close_offset=10`) make the **proof
window 10 blocks wide** (~10 min at ~64s/block). A single large operator with
many supplier keys dumps its entire proof set (N keys × batches of up to
`MaxProofsPerBatch=10`) into the mempool at the same height, on top of every
other operator doing the same. Per-block gas capacity caps how many large proof
txs are included; tx priority is effectively flat (all proofs pay
`gas × 0.000001upokt`). If the burst exceeds ~10 blocks of capacity, the tail is
evicted/never included → forfeited.

Combined with SYNC + no inclusion check (above), the tail is **silently** lost.
Bigger operator → bigger burst → larger tail → matches the observed
distribution where forfeits cluster on a few high-volume operators.

## Empirical confirmation (mainnet, 2026-06-14)

Settlement block `795513` (`pocket` mainnet, queried via `sauron-rpc`):

- 2 `EventClaimExpired` with `expiration_reason="PROOF_MISSING"`, both `eth`,
  ~16.4 + 16.7 POKT forfeited. Suppliers `pokt19halr7cur…`, `pokt1s9v3d6xc7…`,
  `session_end=795480`.
- Both suppliers **did** land their claim (`MsgCreateClaim` @ 795494, code 0)
  but had **zero `MsgSubmitProof` txs on-chain** anywhere in `[795480, 795540]`
  → proof never included (not rejected — absent).
- Network-wide, the 100 most-recent `MsgSubmitProof` were **all code 0** →
  included proofs essentially never fail at DeliverTx. The forfeit mode is
  purely **non-inclusion**.

The decisive observation — proof txs per height across the window for these
sessions (`proofWindowOpen=795503`):

```
h=795504:  233   <- every proof in the window, in ONE block
h=795505..795514: 0   <- ten empty blocks of available proof capacity
```

Block `795504`: 233 txs, gas_used 777M, **block max_gas = -1 (uncapped)**. The
window was not capacity-starved — 10 empty blocks followed the burst. The
forfeited proofs simply missed the single burst block and were **never
re-submitted**, because the miner fires once at window-open and has no
inclusion check / retry. A single rebroadcast into 795505 would have landed
them.

## Reproduction (tests in this repo)

```bash
# Asymmetry: claims are reconciled against chain, proofs are not.
go test -tags test ./miner/ -run TestSilentForfeit -v

# tx layer: SYNC code-0 (mempool accept) is reported as success with zero
# inclusion verification; only CheckTx-level rejections are surfaced.
go test -tags test -race ./tx/ \
  -run 'TestSubmitProofs_SyncAcceptIsSuccess_NoInclusionCheck|TestSubmitProofs_OnlyCheckTxErrorsAreSurfaced' -v
```

- `miner/proof_silent_forfeit_test.go`
  - `TestSilentForfeit_ClaimNonInclusionIsDetected` (control): a
    CheckTx-accepted-but-never-included claim flips to `on_chain_missing` — the
    operator can see that forfeit.
  - `TestSilentForfeit_ProofNonInclusionIsInvisible` (bug): the identical
    situation for a proof leaves `ProofSuccess=true` and the record JSON has no
    `proof_on_chain*` field — the forfeit is invisible.
- `tx/proof_inclusion_test.go` — proves success == mempool acceptance and the
  client issues no `GetTx` (no inclusion verification).

## Confirm on a live miner

The submission tracker records `proof_success=true` at mempool acceptance, not
on-chain (`miner/submission_tracker.go:16-19,49`). To distinguish:

```bash
# Get the proof_tx_hash the miner believes it submitted for a forfeited session.
pocket-relay-miner redis submissions --supplier <addr> --failed-only
# or:  redis submissions --supplier <addr> --session <id> --session-end <h>

# Ask the chain whether that tx actually landed.
pocketd query tx <proof_tx_hash>
#   "tx not found"  -> proof never included  => confirms silent non-inclusion
#   found + failed  -> different bug; read the RawLog
```

Cross-check: `ha_*_proof_submitted` (mempool) vs proofs actually on-chain (or
the `handleClaimExpired` count). The gap is the silent-forfeit rate. Also check
whether forfeited proofs share a single `proof_submit_height` (== window open)
— confirms the burst.

## Fix directions (priority order)

1. **Proof inclusion tracking, symmetric to claims. ✅ IMPLEMENTED** (this
   branch). `ProofInclusionTracker` (`miner/proof_inclusion_tracker.go`) polls a
   new uncached `GetProof` (`query/query.go`) after each proof broadcast and,
   while the proof window is still open and the proof is missing, re-broadcasts
   it (bounded by `MaxRebroadcasts`). Outcome is recorded via the new
   `ProofOnChainOutcome`/`UpdateProofOnChainOutcome`
   (`miner/submission_tracker.go`) and `ha_miner_proof_inclusion_outcome_total`
   / `ha_miner_proof_rebroadcasts_total` metrics. Wired in
   `miner/lifecycle_callback.go` (success branch) + `miner/supplier_manager.go`.
   Tests: `miner/proof_inclusion_tracker_test.go`. This is the direct fix — it
   would have landed both forfeits in the empirical example into block 795505.
2. **Re-enable submission spread for proofs**, staggered across keys, with a
   safe-deadline floor (the reason it was disabled). Stops a single operator
   from self-congesting one block.
3. **Confirm on-chain, not mempool** — treat "submitted" as included, not
   CheckTx-accepted.
4. Tune batch size / fee headroom near congested settlement-adjacent heights so
   partial inclusion is possible.
