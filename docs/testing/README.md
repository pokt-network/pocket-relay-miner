# Testing the RelayMiner

How to exercise the relayer and miner on the local Tilt environment. There are
two guides:

| Guide | What it's for |
|---|---|
| **[TILT.md](TILT.md)** | Bring the localnet up in kind, confirm it's healthy, the port map, and the HA/chaos suite. **Start here.** |
| **[DIRECT_CLI.md](DIRECT_CLI.md)** | Signed relays sent **straight to the relayer** (`:8180`) with the `relay` CLI — correctness, error paths, and load. |

For how the relayer and the miner GET their supplier keys — the two sources, the
keyring backends, the passphrase, and hot reload — see
**[../SUPPLIER_KEYS.md](../SUPPLIER_KEYS.md)**.

## Which one do I want?

- **First time / nothing running** → [TILT.md](TILT.md) to bring it up.
- **Everything else** — throughput, lifecycle → claims → proofs, per-transport
  correctness, error paths → [DIRECT_CLI.md](DIRECT_CLI.md). It covers the five
  transports (JSON-RPC, WebSocket, gRPC, streaming, CometBFT) and carries
  sustained load with `--load-test`.

> **Do not measure relays through the PATH gateway.** PATH answers a relayer
> `503` with `200` and an empty body, so any tool that reads status codes counts
> relays that were never mined: a real run reported `20000/20000 OK` with the
> WAL at `XLEN 0`. This is not confined to error paths — it invalidates
> throughput and lifecycle numbers too. Send relays with the `relay` CLI at
> `:8180`, which verifies the supplier signature and the backend's own error
> field, so a failure reads as a failure.

## Reference material

These are not testing walkthroughs but you'll want them while verifying results:

- [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) — claim/proof window
  timing and the block-driven inclusion reconciler.
- [../CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md) — how relays map to SMST
  leaves and on-chain relay counts (why a claim can show fewer relays than you
  sent).
- [../REDIS.md](../REDIS.md) — Redis key patterns and the `redis` debug
  subcommand in depth.
- [../../scripts/loadtest/README.md](../../scripts/loadtest/README.md) —
  backend RPS-ceiling measurement and per-service connection-pool tuning.
