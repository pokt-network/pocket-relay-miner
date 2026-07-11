## Cache cleanup-all command — design

Date: 2026-07-10
Status: approved
Branch: `feat/redis-cache-cleanup-all`

## Problem

Operators need a single command to wipe every regenerable cache entry in
Redis (e.g. after the contaminated-supplier incident reported by an
operator: persistent `supplier cache entry contaminated` warnings from
stale `ha:supplier:*` entries no reconcile loop ever rewrites). Today the
only options are per-type `redis cache --type X --invalidate --all` calls
or the dangerous generic `redis flush --pattern`, which can also delete
state (sessions, SMST, WAL) and lose money.

The command must be safe to run **while relayers and miners are live and
serving traffic**.

## Decision

Extend the existing subcommand with a pseudo-type:

```
pocket-relay-miner redis cache --type all --invalidate --all [--dry-run|--yes]
```

No new subcommand; reuses existing flags, confirmation flow and dry-run.

## Deletion scope (hot-safe by construction)

| Pattern | Action | Why |
|---|---|---|
| `ha:cache:*` except `ha:cache:lock:*` | DELETE | Regenerable from chain via L3 on next read/refresh |
| `ha:cache:lock:*` | PRESERVE | Deleting a held repopulation lock breaks mutual exclusion during the post-cleanup L3 refill; locks self-expire via TTL |
| `ha:supplier:*` contaminated entries (`Staked:true, Status:active, Services:[]`) | DELETE | Already treated as cache misses by the read guard; deleting changes no traffic behavior |
| `ha:supplier:*` healthy entries | PRESERVE | `relayer/proxy.go` returns 503 on supplier cache miss (fail-open covers Redis errors only, not misses). Wiping healthy entries opens a ≤60 s 503 window until the miner reconcile rewrites them |
| `ha:suppliers:*`, `ha:miner:*`, `ha:smst:*`, `ha:relays:*`, `ha:tx:*`, service_factor, leader keys | NEVER TOUCHED | State, not cache. Patterns don't overlap (`ha:supplier:` ≠ `ha:suppliers:` at the 12th byte) |

Full supplier wipe stays available as the explicit
`--type supplier --invalidate --all`, whose confirmation prompt must warn
about the 503 window.

## Flow

1. SCAN (never KEYS) both patterns; classify keys; parse supplier values
   to detect contamination.
2. `--dry-run`: print per-type breakdown incl.
   `supplier: X contaminated (would delete), Y healthy (preserved)`; delete
   nothing.
3. Interactive confirmation unless `--yes` (same pattern as `invalidateAll`).
4. DEL in pipelined batches of 500.
5. **After** deletion, publish `{}` (clear-all payload) to seven channels:
   the six EventChannel types (`application`, `service`, `supplier`,
   `account`, `shared_params`, `proof_params`) plus the supplier_params
   cache's nonstandard channel
   `{EventsCachePrefix}:invalidate:supplier_params` (its L1 has no TTL) —
   clears L1 fleet-wide immediately. Publishing before deletion would let
   L1s repopulate from a half-deleted L2. The clear-all is also published on
   a zero-key run so a re-run after a crash between the delete and publish
   phases still notifies subscribers.

   Deliberately NOT notified: the relayer's height-keyed
   `RedisSharedParamCache` only parses numeric per-height payloads (no
   clear-all exists); its L2 keys are height-immutable, regenerable via L3,
   and its L1 TTL is one block — safe to skip.

   **Bug found and fixed during implementation**: the `{}` clear-all branch
   in the four entity caches (`application`, `service`, `account`,
   `supplier`) was dead code — `json.Unmarshal("{}", &event)` succeeds, so
   the check living inside the unmarshal-error branch never ran, and the
   clear-all silently did nothing. `handleInvalidation` now checks
   `payload == "{}"` before unmarshalling. The params singleton caches
   were already correct (they clear+reload L1 on any payload).
6. Print summary: deleted keys per type + channels notified.

`session_params` has no invalidation subscriber; its L2 key is deleted via
the cache pattern and its L1 ages out via TTL.

Flag validation: `--type all` requires `--invalidate --all`; `--key` and
`--key-file` are rejected with a clear error.

Errors: SCAN/DEL failure aborts with wrapped error; publish failure warns
and continues (matches existing behavior).

## Live-execution cost (documented in help + confirmation prompt)

First read per entity after cleanup pays L3 (~100 ms) until repopulated;
under production load this is a brief latency burst, bounded by the
repopulation locks we deliberately preserve. No 503s: every relayer cache
consumer except supplier has an L3 fallback, and supplier healthy entries
are preserved.

## Testing (three levels, all required)

1. **Unit (miniredis)**: dry-run deletes nothing; deletes `ha:cache:*` and
   contaminated suppliers; preserves locks, healthy suppliers, and
   negative-control state keys (`ha:suppliers:*`, `ha:miner:*`,
   `ha:smst:*`); publishes `{}` to all six channels; invalid flag combos
   rejected; zero-key run exits cleanly.
2. **Integration**: real `SupplierCache` + `ApplicationCache` over
   miniredis, concurrent reads under the race detector while cleanup runs:
   zero errors, L1 cleared by the `{}` event, misses hit an L3 stub,
   healthy supplier entries keep serving (no 503 window).
3. **Live (Tilt localnet)**: `scripts/test-cache-cleanup-live.sh` —
   continuous load (~200 RPS) against the relayer, run the cleanup without
   dry-run, assert zero rejected relays during/after, caches repopulate,
   claim/proof pipeline uninterrupted.
