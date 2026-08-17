# Test & Utility Scripts

Integration and load-test scripts that run against a live Tilt localnet.

**How to test is documented in [`../docs/testing/`](../docs/testing/README.md):**

- [`TILT.md`](../docs/testing/TILT.md) — bring the localnet up, port map, and the HA/chaos suite.
- [`DIRECT_CLI.md`](../docs/testing/DIRECT_CLI.md) — signed relays straight to the relayer via the `relay` CLI.

> **Scripts that drive PATH (`:3069`) cannot tell you whether relays were
> mined.** PATH answers a relayer `503` with `200` and an empty body, so a
> status-code check reports success either way — a real run showed
> `20000/20000 OK` with the WAL at `XLEN 0`. Where a script below sends traffic
> through PATH, treat its success count as "the gateway replied", and confirm
> the outcome in Redis (`redis streams`, `redis submissions`) or with the CLI at
> `:8180`.

This file is just an inventory of what's here. All scripts assume the
`kind-kind` context and the localnet defaults (PATH `:3069`, relayer `:8180`,
Redis `:6379`, Prometheus `:9091`, Loki `:3100`). The `redis` subcommand's
`--redis` flag defaults to `redis://localhost:6379`, so it can be omitted.

## HA / chaos / resilience

| Script | What it does |
|---|---|
| `test-chaos.sh` | Chaos monkey: kills relayer/miner pods, blips Redis, injects backend latency/partitions/memory pressure. |
| `test-chaos-leader-flush-gap.sh` | Kills the miner leader in the claim-flush→proof gap; regression guard for SMST lazy-load on failover. |
| `test-quantitative-failover.sh` | Mid-flight miner scale-down; asserts on-chain claimed relays == loader successes within a drift budget. |
| `verify-rebalance-fix.sh` | Rebalancer veto regression (issue #7): both miner replicas must end with non-zero claimed_count. |
| `verify-claim-payment.sh` | End-to-end claim payment: sends relays, watches the claim tx, confirms `EventClaimSettled` on-chain. |

## Load / lifecycle (PATH + hey)

| Script | What it does |
|---|---|
| `test-continuous-load.sh` | Sustained ~200 RPS (override `--rps`) until Ctrl-C; for memory/CPU leak watching. |
| `test-multi-supplier-10k.sh` | 10k relays with no supplier pinning; forces concurrent claim/prove across every supplier. |
| `test-stress-max.sh` | ~1000 RPS HTTP + 200 concurrent WebSocket for ~5 min; asserts claims/proofs via Loki. |
| `test-smst-claim-and-ws.sh` | HTTP claim/proof load + concurrent WebSocket writes (SMST-root + WS-mutex regression). |
| `test-round-robin.sh` | Tallies `result.backend_id` to confirm even distribution across the multi-backend HTTP pool. |

## Direct CLI / feature validation

| Script | What it does |
|---|---|
| `test-cache-cleanup-live.sh` | Level-3 validation of `redis cache --type all` cleanup under sustained CLI-driven load (direct to `:8180`, `--all-suppliers`). |
| `test-inclusion-reconciler.sh` | Verifies the block-driven inclusion reconciler; asserts on-chain inclusion via `redis submissions` + Prometheus/Loki. |

## Subdirectories

- `loadtest/` — backend RPS-ceiling measurement and per-service pool tuning (`backends.sh`, `http-verify.go`). See [`loadtest/README.md`](loadtest/README.md).
- `ws-test/` — manual WebSocket tester (session rollover, not covered by the CLI `relay websocket` mode).
- `ws-stress/` — WebSocket memory/leak stress tester.
- `lib/` — shared bash helpers (`common.sh`).
- `localonly/` — gitignored; operator-specific config (see the operator-data rule in [`../CLAUDE.md`](../CLAUDE.md)).
