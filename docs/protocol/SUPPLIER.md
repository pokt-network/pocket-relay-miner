## Supplier — the protocol rules

Verified against **poktroll v0.1.35** (the version `go.mod:20` pins). Each
rule cites the file it comes from. **Without a citation, it is not a rule**: it is an
assumption, and this document exists because assumptions about the protocol
cost us a weekend chasing relays that were not missing.

The `poktroll/...` paths are relative to the module root in the module cache.

### What a supplier is, and which fields define it

`poktroll/x/shared/types/supplier.pb.go`:

| field | what it is |
|---|---|
| `operator_address` | who operates. It is the identity that signs relays |
| `owner_address` | who put up the money and whom it returns to |
| `stake` | the stake |
| `services` | the declared services |
| `service_config_history` | the history, with `activation_height` and `deactivation_height` per entry |
| `unstake_session_end_height` | the session in which it finishes unbonding |

**The history is not decoration: it is what decides whether the supplier is active.**

### Rule 1 — "active" is PER SERVICE and PER HEIGHT, and does NOT look at unbonding

`poktroll/x/shared/types/supplier.go:23-38` — `Supplier.IsActive(queryHeight, serviceId)`
walks `ServiceConfigHistory` and returns true if there is an entry for THAT
service whose window contains THAT height (`activation <= h < deactivation`).

**It does not consult `unstake_session_end_height`.** Unbonding and being active
for a service are **orthogonal** in the protocol.

`IsUnbonding()` is a different and poorer question
(`poktroll/x/shared/types/supplier.go:10-12`): it only says whether
`unstake_session_end_height != SupplierNotUnstaking`.

**Consequence for this repo**: our `cache.SupplierState.IsActive()` is
`Staked && Status != NotStaked` (`cache/supplier_cache.go`), which is COARSER than
the protocol's — it is neither per service nor per height. The two names match and
the two questions do not. When stating something about "active", say which of the two.

### Rule 2 — a supplier that is unbonding KEEPS SERVING

It follows from rule 1: if its service configuration is still active at that
height, it serves, even if `IsUnbonding()` is true.

**This already cost us a defect**: the drain write rebuilt a partial state
and erased the transports view of a supplier that was still serving
(fixed in `b67b811`).

### Rule 3 — there are FOUR unbonding reasons, not one

`poktroll/x/supplier/types/event.pb.go`:

- `SUPPLIER_UNBONDING_REASON_VOLUNTARY` — the operator requested it
- `SUPPLIER_UNBONDING_REASON_BELOW_MIN_STAKE` — **the chain unstaked it on its own**
- `SUPPLIER_UNBONDING_REASON_MIGRATION`
- `SUPPLIER_UNBONDING_REASON_UNSPECIFIED`

**The second is the one that gets forgotten**: a supplier can enter unbonding **without
anyone on the operator side doing anything**, by falling below the minimum stake (for
example after a slash). Any reasoning that assumes "unbonding = someone
requested unstake" is incomplete.

### Rule 4 — the SIX events, which is what can be observed

`poktroll/x/supplier/types/event.pb.go`:

| event | when |
|---|---|
| `EventSupplierStaked` | it was staked |
| `EventSupplierUnbondingBegin` | it started unbonding |
| `EventSupplierUnbondingEnd` | it finished |
| `EventSupplierUnbondingCanceled` | **it was canceled** — unbonding is reversible |
| `EventSupplierServiceConfigActivated` | a service config was activated |
| `EventSupplierStakeStuckInModulePool` | see rule 5 |

`UnbondingBegin` and `UnbondingEnd` carry the same four fields: `supplier`,
`reason`, `session_end_height`, `unbonding_end_height`.

**`EventSupplierUnbondingCanceled` exists**, so an observed unbonding is NOT
a terminal state and cannot be treated as one.

### Rule 5 — returning the stake CAN FAIL, and the money gets stuck

`poktroll/x/supplier/keeper/unbond_suppliers.go:88-106`. If sending the
coins from the module pool to the owner's account fails, **the supplier is
removed anyway and the coins stay in the module pool**, and
`EventSupplierStakeStuckInModulePool` is emitted "for indexer/governance".

Verbatim from the code: *"supplier will be removed and coins will remain in module
pool"*.

**Consequence**: "the supplier finished unbonding" does **not** imply "the owner
got paid". They are two distinct facts and there is an event dedicated exactly to the case
where they diverge.

### Rule 6 — a supplier that is UNBONDING IS STILL SELECTED for new sessions

`poktroll/x/session/keeper/session_hydrator.go:187-226` — `hydrateSessionSuppliers`
builds the candidates **only** from the service configuration iterator
(`GetServiceConfigUpdatesIterator(serviceId, blockHeight)`) and keeps those that
satisfy `IsActive(blockHeight)`.

**It does not consult `IsUnbonding()` or `unstake_session_end_height` at any point.**

Consequences, and they are about money:

- A supplier that is unbonding **keeps being assigned relays**. Serving them
  and claiming them is correct; failing to keep its local state right is not.
- What finally takes it out of sessions **is not the unbonding**: it is that on
  finishing it is REMOVED from state (`unbond_suppliers.go`), which takes its
  service configurations with it and therefore removes it from the iterator.
- `NumSuppliersPerSession` is read with `GetParamsAtHeight(ctx, blockHeight)`, that is,
  **historical parameters**, so that hydrating a past session is deterministic.
- If there is not a single candidate, hydration **fails** with
  `ErrSessionSuppliersNotFound`; it does not return an empty session.

### Rule 7 — when unbonding ends, with the formula

`poktroll/x/shared/types/supplier.go:75-84`:

```
unbondingEndHeight = unstake_session_end_height
                   + SupplierUnbondingPeriodSessions * NumBlocksPerSession
```

Default of `SupplierUnbondingPeriodSessions`: **1 session**
(`poktroll/x/shared/types/params.go:20`). **It is the code's default, NOT the mainnet
value** — that one must be queried from the chain.

The unbonding queue skips those that are not unbonding (`unbond_suppliers.go:43`)
and those that have not reached their height yet (`:62`).

### Rule 8 — on finishing, the money may NOT move, for two distinct reasons

`poktroll/x/supplier/keeper/unbond_suppliers.go:80-106`:

1. **Stake at 0 due to slashing** → *nothing moves*, and it is deliberate: the code
   checks `supplier.Stake.IsPositive()` precisely to avoid transferring 0 coins.
2. **The transfer fails** (owner that is a module account, legacy state
   predating v0.1.34) → it is logged, `EventSupplierStakeStuckInModulePool` is emitted
   and **it continues on purpose**. Verbatim: *"Why not halt the chain: pre-existing
   legacy state must not be allowed to brick the EndBlocker"*. The supplier is
   removed anyway and the coins stay in the module pool.

That is: **"finished unbonding" does not imply "the owner got paid"**, and there are TWO
distinct paths by which they do not get paid.

### Rule 9 — service configurations are activated when a session STARTS

`poktroll/x/supplier/keeper/activate_services.go:30-60`. When a session starts,
`GetActivatedServiceConfigUpdatesIterator(currentHeight)` is walked and an
`EventSupplierServiceConfigActivated` is emitted **for each activated configuration**, with
`ActivationHeight: currentHeight`.

Orphaned index entries are **skipped with a debug log**, they do not fail
(`:48-52`).

### The only thing that CANNOT be read from the code

The **mainnet value** of `supplier_unbonding_period_sessions` and of
`num_blocks_per_session`: they are chain state, not source. They are queried from a
node. The code's default (1 session) is **not** evidence of what runs on
mainnet.
