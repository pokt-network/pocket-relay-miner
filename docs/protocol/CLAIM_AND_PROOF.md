## Claim and Proof — the protocol rules, and where the money moves

Verified against **poktroll v0.1.35** (`go.mod:19`). Every rule cites its source.
**Without a citation, it is not a rule.**

### Rule 1 — the Claim has FOUR fields, and the amount is NOT one of them

`poktroll/x/proof/types/types.pb.go`, `type Claim struct`:

| field | |
|---|---|
| `supplier_operator_address` | who claims |
| `session_header` | which session |
| `root_hash` | **the root of the tree that WE built and signed** |
| `proof_validation_status` | see rule 4 |

There is no `num_relays`. There is no `num_compute_units`. **No amount is stored.**

### Rule 2 — the amount is DERIVED from our root, so the chain is not an oracle

`poktroll/x/proof/types/claim.go:20-22` and `:32-34`:

```go
func (claim *Claim) GetNumClaimedComputeUnits() (uint64, error) {
	return smt.MerkleSumRoot(claim.GetRootHash()).Sum()
}
func (claim *Claim) GetNumRelays() (uint64, error) {
	return smt.MerkleSumRoot(claim.GetRootHash()).Count()
}
```

`num_relays` is the **Count** and the claimed units are the **Sum** of the same root
we send. The chain **reads what we gave it**; it does not count relays on its own.

**Consequences, and they are the ones most often confused:**

- **Comparing "relays served" against the chain's `num_relays` does NOT validate the
  chain: it validates our tree against itself.** If the tree loses a leaf, the chain
  reports the lost number without noticing anything.
- **Underclaiming is invisible by design.** Claiming too little produces a perfectly
  valid claim.
- The honest comparison against the chain is **SMST leaves vs `num_relays`**.

### Rule 3 — not every relay enters the tree, and that is NOT a loss

Verbatim comment in `claim.go:24-31`: *"not every Relay (Request, Response) pair
in the session is inserted into the tree. The relay hash has to have matched the
difficulty for that service"*, and it explains why: *"controlled by the Relay
Mining difficulty to reduce co-processor hardware requirements and enable scaling
to tens of billions of relays"*.

**Consequence**: served > leaves is NORMAL when the difficulty is not the base one.
There are **three different counts** —served, tree leaves, `num_relays`— and only
the last two must match.

That is why the event carries `num_relays` **and** `num_estimated_relays`, and
`num_claimed_compute_units` **and** `num_estimated_compute_units`
(`event.pb.go`, `EventClaimUpdated`): what is claimed is what entered the tree; what
is estimated is what that tree represents given the difficulty.

### Rule 4 — two different enums, and they get confused

`poktroll/x/proof/types/types.pb.go`:

- **`ClaimProofStage`**: `CLAIMED`, `PROVEN`, `SETTLED`, `EXPIRED` — where the claim
  is in its lifecycle.
- **`ClaimProofStatus`**: `PENDING_VALIDATION`, `VALIDATED`, `INVALID` — what was
  concluded about its proof.

The field that lives IN the claim is `proof_validation_status` (the second one).

### Rule 5 — the FIVE events of the proof module

`poktroll/x/proof/types/event.pb.go`: `EventClaimCreated`, `EventClaimUpdated`,
`EventProofSubmitted`, `EventProofUpdated`, `EventProofValidityChecked`.

`EventClaimUpdated` carries: `num_relays`, `num_claimed_compute_units`,
`num_estimated_compute_units`, `num_estimated_relays`, `claimed_upokt`,
`service_id`, `application_address`, `session_end_block_height`,
`supplier_operator_address`, `claim_proof_status_int`.

**`claimed_upokt` is in the event**: the claimed money can be read from the event
without recomputing it.

### Rule 6 — the proof is not always required, and THREE parameters decide it

`poktroll/x/proof/types/params.pb.go`: `ProofRequestProbability`,
`ProofRequirementThreshold`, `ProofMissingPenalty`.

That is: above a threshold the proof is always required; below it, it is required
with a given probability. **A claim without a proof is not necessarily wrong.**

### Rule 7 — the settler is the tokenomics module, and that is where slashing is

`poktroll/x/tokenomics/keeper/settle_pending_claims.go:296` calls
`slashSupplierStake` for the EXPIRED outcome, and `:720-728` is the function.

**Consequence**: the penalty for a missing or invalid proof does **not** come from
the proof module: it comes from settlement. An expired claim is the one that costs
stake.

### What cannot be read from the code

The mainnet values of `ProofRequestProbability`, `ProofRequirementThreshold`
and `ProofMissingPenalty`. They are chain state.

### Rule 8 — the Proof is EPHEMERAL: it is validated and deleted in the same EndBlocker

`poktroll/x/proof/keeper/validate_proofs.go:46` is `ValidateSubmittedProofs`, which
runs in EndBlocker context (its own comment about the gas meter says so,
`:85-93`). It validates each proof in a goroutine, waits for all of them, closes the
iterator and then:

> *"Delete all the processed proofs from the store since they are no longer
> needed."* (`:104-110`, with `k.RemoveProof(ctx, sessionId, supplierOperatorAddr)`)

**Consequence, and it is an expensive trap**: querying the proof to find out whether
it was included **does not work** — by the time you ask, it no longer exists.
Inclusion is verified via **`Claim.ProofValidationStatus`**, which does persist in
the claim.

An `AllProofs` or a `GetProof` that returns empty means *"already processed"*, not
*"never arrived"*. Both cases give the same result and **cannot be told apart that
way**.
