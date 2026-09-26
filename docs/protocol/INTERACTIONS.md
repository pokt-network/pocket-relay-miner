## How it all interacts — the life of a relay, and where each parameter bites

Verified against **poktroll v0.1.35**. The per-entity sheets sit next to this one;
this is what **none of them can show on its own**: the order, and what depends on what.

### The money chain, end to end

```
relay served
  └─ does its hash meet target_hash?         ← service, difficulty
       no → does NOT enter the tree (and is NOT a loss)
       yes → SMST leaf
              └─ root signed by US
                   ├─ Count() = num_relays
                   └─ Sum()   = num_claimed_compute_units
                        └─ × multiplier(target_hash)        ← service
                             = num_estimated_compute_units
                                  └─ × CUTTM / granularity  ← shared
                                       = claimed_upokt
                                            └─ bounded by the B/N floor ← application
                                                 └─ × 0.7 split         ← tokenomics
                                                      = what the supplier collects
```

**Each arrow is a place where the number legitimately goes down.** Before calling
a difference a "loss", you have to know in which of the five it happened.

### The five legitimate cuts, in order

1. **The difficulty** decides which relay enters the tree (`SERVICE.md`, rule 2). At
   base difficulty everything enters and the multiplier is 1.
2. **The multiplier** raises the number again to estimate the real one
   (`SERVICE.md`, rule 3). It is the inverse of the previous one, not a cut.
3. **CUTTM / granularity** converts compute units into uPOKT
   (`claim.go:GetClaimeduPOKT`). It is the price, and it is **network-wide**.
4. **The `B/N` floor** of the application bounds what can be charged to its
   budget (`APPLICATION.md`, rule 1). Overservicing can be paid from the
   surplus, bounded by `overservicing_bonus_multiplier`.
5. **The emission split** leaves the supplier **0.7** (`PARAMS.md`,
   tokenomics). The remaining 0.3 goes to the DAO, proposer and source owner **by design**.

### The schedule, and which parameter each step depends on

```
session ─── sessionEnd
             +ClaimWindowOpenOffset +1  → CLAIMING is allowed
             +ClaimWindowCloseOffset    → it closes
             +ProofWindowOpenOffset     → PROVING is allowed
             +ProofWindowCloseOffset    → it closes; after that the claim EXPIRES
                                          and expiring COSTS STAKE
```

The four offsets belong to `x/shared` and **are governance parameters**: the miner's
schedule is not ours, the chain sets it and it can change without us touching code.

**And there is no spreading across suppliers**: the function that did it is commented
out (`SESSION.md`, rule 1), so we can all submit at the same height.

### Who decides what, which is where the most confusion happens

| the question | the entity that answers it | the one that does NOT |
|---|---|---|
| does this supplier serve this service now? | **supplier**, via `ServiceConfigHistory` and height | not the unbonding |
| does it enter the session? | **the service config window** | not the unbonding (`SUPPLIER.md`, rule 6) |
| how much is a relay worth? | **service** (CUPR) + `shared` (CUTTM) | not the supplier |
| how many relays count? | **the root we sign** | not the chain (`CLAIM_AND_PROOF.md`, rule 2) |
| which gateways can an app use? | **the application** (`delegatee_gateway_addresses`) | not the gateway (`GATEWAY.md`) |
| how much can be charged? | **the application** (budget and floor) | not the claim |
| how much reaches the supplier? | **tokenomics** (0.7 split) | not the claim |

### The three shapes repeated across the three stakeable entities

`supplier`, `application` and `gateway` share structure, and learning it once
serves for all three:

1. All three have `stake` and `unstake_session_end_height`.
2. All three have **`UnbondingCanceled`**: unbonding **is reversible** and is never
   a terminal state.
3. `supplier` and `application` have **`StakeStuckInModulePool`**: finishing
   unbonding **does not imply the owner got paid**.

### What a governance change can do to us without warning

- **Moving the four offsets** → changes when we must claim and prove.
- **Moving `target_num_relays`** → moves the difficulty → changes how many relays
  enter the tree, with the same traffic.
- **Moving CUTTM or the granularity** → changes the price of everything not yet settled.
- **Moving `mint_allocation_percentages`** → changes our 0.7.
- **Moving the supplier `min_stake`** → can put us into unbonding **on its own**.
- **Moving `num_suppliers_per_session`** → bounds `N`. On chain, `N` in the `B/N`
  floor is the number of suppliers that actually claimed (`APPLICATION.md`,
  rule 1), at most `num_suppliers_per_session`: **more suppliers claiming, a
  smaller floor for each.** Our relay meter is stricter: its guaranteed share
  (`baseLimit`, `config.miner.example.yaml`) divides by
  `num_suppliers_per_session` itself, because it cannot know in advance who will
  claim.

None of those requires us to change code, and all of them change the money.

### What is NOT read yet

The rules for a **delegation that changes mid-session** and how the signing ring
is assembled (see `GATEWAY.md`). It is the only piece of this series still without
citations, and it is declared as such instead of completed from memory.
