## Claim & proof lifecycle

How the miner turns served relays into on-chain rewards, and how it guards
against a claim/proof that is accepted to the mempool but never lands in a block.

### On-chain windows

Sessions are aligned to a fixed block grid (`num_blocks_per_session`). For a
session ending at height `E`, the protocol opens two windows (offsets are
`x/shared` params):

```
session ends (E) ──► claim window [open … close] ──► proof window [open … close] ──► settlement
```

- **Claim window**: the supplier must submit a `MsgCreateClaim` (the SMST root +
  relay/compute counts).
- **Proof window**: if a proof is required (`proof_request_probability` /
  `proof_requirement_threshold`), the supplier must submit a `MsgSubmitProof`
  (one closest-merkle-proof against the claimed root).
- **Settlement** (just after proof-window close): the claim is paid, or
  forfeited (`CLAIM_MISSING` / `PROOF_MISSING`) / slashed if its proof was
  missing or invalid.

### Miner flow (per session)

1. **Accumulate** — relays are validated by the relayer and streamed into the
   session's SMST in Redis.
2. **Claim** — at claim-window open the SMST is flushed (root computed) and a
   batched `MsgCreateClaim` is submitted for the session(s) ending at `E`.
3. **Prove** — at proof-window open the closest proof is built from the SMST and
   a `MsgSubmitProof` is submitted.
4. **Settle** — on success the SMST and session state are cleaned up.

Submission is **batched by session-end height** (one tx per supplier per window,
not one per session). There is no switch to disable it: 1 transaction per
session would flood the node at scale and is a primary cause of missed windows.

### Inclusion reconciler (the in-window safety net)

A code-0 `BroadcastTx` only means *mempool acceptance*, not block inclusion. If a
claim/proof misses its block and is never re-sent it is silently forfeited at
settlement even while later window blocks are empty. The **block-driven inclusion
reconciler** closes this gap. Once per block, for each supplier this replica
owns, it:

1. Reads what was submitted this window (the built `MsgCreateClaim` /
   `MsgSubmitProof`, persisted to Redis at submit time).
2. Checks on-chain inclusion (see below).
3. **Present** → record `on_chain_found`, drop the persisted message.
   **Missing + window open** → re-broadcast (by default on every block the
   window allows; see the cadence below).
   **Missing + window closed** → record `on_chain_missing`.

Inclusion is resolved from **x/proof module state**, never the tx indexer, so it
works on nodes running `tx_index=null`:

- **Claim inclusion** = a claim exists for the supplier (`AllClaims` by the
  supplier secondary index). Claims persist until settlement.
- **Proof inclusion** = the claim's `ProofValidationStatus == VALIDATED`. A
  submitted proof is validated and **deleted in the EndBlocker of its submission
  block** (`x/proof` runs `ValidateSubmittedProofs` every block), so it is not
  queryable afterwards — the durable signal lives on the claim, which the
  EndBlocker marks `VALIDATED` / `INVALID` (else `PENDING_VALIDATION`).

Rebroadcast cadence: by default there is **no cap**. A claim or proof still
missing is re-sent on every block after the one it was submitted in, up to the
block before its window closes (`rebroadcast_safety_blocks`, default 0, moves
that earlier). Each re-send costs 1 transaction fee. `max_rebroadcasts` caps the
re-sends per session (`0` = observe only: verify and record, never re-send). The
resend count is persisted, so the cap holds across blocks and across failover.

### HA

All state lives in Redis: the submission record, the persisted built messages,
and on-chain truth (queried fresh each block). Coordination is **per-supplier
ownership** (a `SetNX` claim lease), not leadership — the reconciler runs on
every replica but acts only on suppliers it owns. If an owner dies, its supplier
leases expire, a surviving replica re-claims them, and that replica resumes the
verify/rebroadcast loop straight from Redis — no lost in-flight rebroadcasts, no
silent forfeit.
