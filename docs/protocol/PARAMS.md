## Governance parameters — the complete inventory, and what each one does to us

Verified against **poktroll v0.1.35** (`go.mod:20`), enumerating each module's
`Params` from the source. **Without a citation, it is not a rule.**

**All of them are governance parameters**: they change by proposal, without us
touching code, and several change **how much we collect**. The defaults below are
**the code's**, not mainnet's — the current value is queried from the chain.

### `x/shared` — 13 parameters, and this is where the price lives

| parameter | what it does to us |
|---|---|
| `compute_units_to_tokens_multiplier` | **the price**. Default `42_000_000` (`params.go:29`) |
| `compute_unit_cost_granularity` | the price divisor: the CUTTM/granularity pair gives the uPOKT of ONE compute unit |
| `num_blocks_per_session` | the session length; it enters the unbonding-end formula |
| `claim_window_open_offset_blocks` | when we can claim |
| `claim_window_close_offset_blocks` | until when |
| `proof_window_open_offset_blocks` | when we can prove |
| `proof_window_close_offset_blocks` | until when. Past this the claim expires and **costs stake** |
| `grace_period_end_offset_blocks` | the session's grace period |
| `supplier_unbonding_period_sessions` | how long one of our suppliers takes to exit |
| `application_unbonding_period_sessions` | same, for whoever pays |
| `gateway_unbonding_period_sessions` | same, for the gateway |
| `session_grid_anchor_height` | the anchor of session numbering |
| `session_number_at_anchor` | same |

**The four window offsets are the miner's entire schedule.** A governance change
there moves when we must claim and prove; see `SESSION.md`.

### `x/session` — 1

| parameter | what it does to us |
|---|---|
| `num_suppliers_per_session` | how many suppliers enter per session. **It is read with HISTORICAL parameters** (`session_hydrator.go:190`), so an old session is rehydrated with the value at THAT height |

### `x/service` — 2

| parameter | what it does to us |
|---|---|
| `target_num_relays` | the target the difficulty EMA converges to: **it moves the `target_hash` and therefore how many of our relays enter the tree** |
| `add_service_fee` | cost of registering a service |

### `x/supplier` — 2

| parameter | what it does to us |
|---|---|
| `min_stake` | below it, the chain **unstakes us on its own**: `SUPPLIER_UNBONDING_REASON_BELOW_MIN_STAKE` (see `SUPPLIER.md`) |
| `staking_fee` | paid when staking |

### `x/application` — 2

| parameter | what it does to us |
|---|---|
| `min_stake` | the floor for whoever pays |
| `max_delegated_gateways` | **bound on the size of the signing ring** |

### `x/gateway` — 1

`min_stake`.

### `x/proof` — 4

| parameter | what it does to us |
|---|---|
| `proof_requirement_threshold` | above it, the proof is **mandatory** |
| `proof_request_probability` | below the threshold, it is **drawn at random** |
| `proof_missing_penalty` | what not having it costs |
| `proof_submission_fee` | what submitting it costs |

**The first two are why a claim without a proof can be fine.**

### `x/tokenomics` — 6, and they decide how much reaches us

| parameter | what it does to us |
|---|---|
| `mint_allocation_percentages` | **the split**. Default: supplier **0.7**, source owner 0.15, DAO 0.1, proposer 0.05, application 0.0 (`x/tokenomics/types/params.go`) |
| `mint_equals_burn_claim_distribution` | the same split for the mint==burn regime. Same defaults |
| `global_inflation_per_claim` | default `0.1` |
| `mint_ratio` | default `1.0` — *"no deflation (mint equals burn)"* |
| `overservicing_bonus_multiplier` | default `1`. **Zero is treated as 1, never as unlimited** (see `APPLICATION.md`) |
| `dao_reward_address` | where the DAO's share goes |

**The rule most often forgotten, and it lives here**: of what is distributed, **the
supplier gets 0.7, not 1.0**. Collecting 70% is not a loss: it is the split.
