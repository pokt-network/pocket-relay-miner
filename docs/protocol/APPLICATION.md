## Application — the protocol rules, and the budget that cuts the payment

Verified against **poktroll v0.1.35** (`go.mod:20`). **Without a citation, it is not a rule.**

The application is **the one that pays**. Its stake is the budget that pays what a
supplier collects, and that is why most of the rules on "why was I paid
less than I claimed" live here and not in the claim.

### The fields

`poktroll/x/application/types`:

| field | why it matters |
|---|---|
| `address` | |
| `stake` | the budget |
| `service_configs` and `service_config_history` | same as in supplier: windows per height |
| `delegatee_gateway_addresses` | the delegated gateways (the signing ring) |
| `pending_undelegations` | delegations being undone |
| `per_session_spend_limit` | **per-session spend limit** |
| `pending_transfer` | see rule 3 |
| `unstake_session_end_height` | same as in supplier |

### Rule 1 — the per-supplier floor is B/N, and it is a FLOOR, not a ceiling

`poktroll/x/tokenomics/keeper/token_logic_modules.go:316-330`, on
`ensureClaimAmountLimits`. Verbatim:

> *"The per-supplier head-split B/N (where B is the application's per-session
> budget and N the actual number of claiming suppliers) is a GUARANTEED FLOOR,
> not a hard ceiling"*

- Serving **at or below** the floor is **always paid in full**.
- Serving **above** it can additionally be paid from the budget left unused by
  idle or light suppliers, **in proportion to one's own excess**.

**`N` is the REAL number of suppliers that claimed**, not the number of suppliers in
the session. A supplier that does not claim enlarges the floor of the others.

### Rule 2 — the overservicing bonus, and the zero that does NOT mean infinite

`token_logic_modules.go:360-375`:

```
bonus_i = unused * excess_i / totalExcess      (integer division ⇒ Σ bonus ≤ unused)
```

Bounded by `overservicing_bonus_multiplier` (`m`):

- `m == 0` or `m == 1` → capped exactly at the floor (legacy behavior, no
  redistribution).
- `m > 1` → allows up to `m * floor` from the unused budget.

**Zero is deliberately treated as 1, NOT as "unlimited"**, and the code says why:
the zero value of the parameter —whether from a fresh proto3 decode, from an upgrade
handler that did not run, or from a write that overwrote it— **must be benign and never
silently enable redistribution**.

And the closing invariant: `Σ floor + Σ bonus ≤ N*floor = B`, so **what the group
settles never exceeds the committed budget and the application's stake cannot go
negative**.

**Consequence for any claim of loss**: being paid less than claimed **is not our
defect by default**. It may be the floor working. Before calling it a loss you need
to know B, N and `m`.

### Rule 3 — an application can be TRANSFERRED, and it fails in three steps

`poktroll/x/application/types/event.pb.go` — nine events:
`EventApplicationStaked`, `EventRedelegation`, `EventTransferBegin`,
`EventTransferEnd`, `EventTransferError`, `EventApplicationUnbondingBegin`,
`EventApplicationUnbondingEnd`, `EventApplicationUnbondingCanceled`,
`EventApplicationStakeStuckInModulePool`.

- The transfer has **begin / end / error** and a `pending_transfer` field: it is
  not atomic and **can fail**.
- `EventApplicationUnbondingCanceled` exists: same as in supplier, **unbonding is
  reversible** and not terminal.
- `EventApplicationStakeStuckInModulePool` exists: **the same trap as in
  supplier** — the stake can get stuck in the module pool.

### What cannot be read from the code

The mainnet values of `overservicing_bonus_multiplier`, of the minimum application
stake, and of the `per_session_spend_limit` of a specific application. They are
chain state.
