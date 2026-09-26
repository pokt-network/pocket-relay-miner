## Service — how much a relay is worth, and which ones enter the tree

Verified against **poktroll v0.1.35** (`go.mod:19`). **Without a citation, it is not a rule.**

The service is the entity that decides **the price of a relay** and **what fraction of
the relays served becomes claimable**. It is where the multiplier that turns tree
leaves into money lives, so almost any claim of the form "we collect less than we
served" is resolved here.

### The fields

`poktroll/x/shared/types/service.pb.go`, `type Service struct`:

| field | |
|---|---|
| `id` | |
| `name` | |
| `compute_units_per_relay` | **CUPR**: how many compute units ONE relay of this service is worth |
| `owner_address` | |
| `metadata` | |

### Rule 1 — the CUPR belongs to the SERVICE, not to the relay or the supplier

A relay does not carry its price: it inherits it from the service. **Changing the CUPR
changes the value of every relay of that service**, and it does so for any session
settled after the change.

### Rule 2 — the difficulty decides which relay ENTERS the tree

`poktroll/x/service/types/relay_mining_difficulty.pb.go`, `type
RelayMiningDifficulty struct`:

| field | |
|---|---|
| `service_id` | |
| `block_height` | |
| `num_relays_ema` | the exponential moving average of the service's relays |
| `target_hash` | **the threshold**: only relays whose hash meets it enter the tree |

See `docs/protocol/claim-and-proof.md`, rule 3: *"not every Relay (Request,
Response) pair in the session is inserted into the tree. The relay hash has to
have matched the difficulty for that service."*

### Rule 3 — the multiplier formula, exact

`poktroll/pkg/crypto/protocol/relay_difficulty.go:85-96`:

```
probability = target_hash / BaseRelayDifficultyHash      (GetRelayDifficultyProbability)
multiplier  = 1 / probability                            (GetRelayDifficultyMultiplier)
```

Verbatim from the code: the multiplier *"scales FROM 'onchain_volume_applicable_relays'
TO 'offchain_estimate_actual_relays'"*.

**Consequences:**

- When `target_hash == BaseRelayDifficultyHash`, the probability is 1 and the
  multiplier is 1: **everything enters, and estimated == claimed**. That is the case
  of the local gates, and that is why there `served == billed` exactly is the correct
  assertion.
- With a difficulty above the base, **estimated > claimed by design**, and the
  difference **is not a loss**.
- `GetRelayDifficultyMultiplierToFloat32` exists but the code says, in
  capitals, *"THIS IS TO BE USED FOR TELEMETRY PURPOSES ONLY"*: the conversion to
  float32 loses precision and **must not be used for money**. What is used is
  `*big.Rat`.

### Rule 4 — estimated = multiplier × claimed, and the service ID must match

`poktroll/x/proof/types/claim.go`, `getNumEstimatedComputeUnitsRat`:

```
numEstimatedComputeUnits = GetRelayDifficultyMultiplier(difficulty.target_hash)
                         × claim.GetNumClaimedComputeUnits()
```

And before multiplying it validates that **the claim's service ID matches the
difficulty's**, or it fails with `ErrProofInvalidRelayDifficulty`.

**Consequence**: the difficulty used is **that of the claim's service**. Applying
another service's difficulty does not give a wrong number: it gives an error.

### Rule 5 — the difficulty CHANGES, and an event reports it with both values

`poktroll/x/service/types/event.pb.go`: `EventRelayMiningDifficultyUpdated`, with
`service_id`, `prev_target_hash_hex_encoded`, `new_target_hash_hex_encoded`,
`prev_num_relays_ema` and `new_num_relays_ema`.

**It is the only event of the service module.** It carries the previous and the new
value, so a difficulty change **can be dated and quantified from the chain** instead
of being inferred.

### What cannot be read from the code

The CUPR of a specific service and the current `target_hash`: they are chain
state. `BaseRelayDifficultyHashBz` is a protocol constant.
