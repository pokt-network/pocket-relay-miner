# Benchmarks

Capacity reports for released versions. Each one is a load run on 1 machine,
kept here so the figures quoted in the other docs have a source you can open.

## v0.1.0

[Relay-Miner-Capacity.pdf](v0.1.0/Relay-Miner-Capacity.pdf) (13 pages).

What it measured:

- **Topology**: 1 relayer, 1 miner and 1 Redis, each in its own container
  (relayer 8 GiB, miner 8 GiB, Redis 16 GiB with `maxmemory` 12.8 GiB), against
  a local chain with ~60 s blocks. The 2 relayers + 2 miners topology was tested
  at lower load and is not in the report.
- **Load**: 6 services on 3 transports (JSON-RPC, WebSocket, gRPC), each
  session served by 50 suppliers, sent by the project's own
  `pocket-relay-miner relay` client straight to the relayer, every response
  verified. **10.7 M relays served at ~2,400 relays/s** on average.
- **Settlement**: **1,501 of 1,501 claims settled** with valid proofs, none
  expired, none slashed, with the miner **killed twice** on purpose (`kill -9`,
  once in a claim window and once in a proof window).
- **A burst**: 128 concurrent 1 MiB requests for 15 s on top of the full load.
- **A comparison** with the build that preceded the release, run on the same
  machine and load.

## How to read it against your load

- The figures come from 1 topology on 1 machine. Supplier count, services,
  validation mode, transports and hardware all change them: use them as a
  reference for sizing, not as a guarantee.
- **Relayer**: size it by CPU. It keeps no state, so more relays/s means more
  cores or more relayers. The ~2,400 relays/s is the load the test chose, not a
  ceiling.
- **Miner and Redis**: size them by the sessions they hold at once, not by
  relays/s. Both follow the number of live claim trees (1 per session and
  supplier), and the protocol's session, claim and proof windows set how many
  overlap. The report's "Sizing this release" section gives the memory per
  number of live trees and explains the peak.
- A burst of large relays adds to the miner's memory on top of that; the 1 MiB
  burst is the thinnest margin in the run.
- The limits shipped in [examples/](../../examples/) fit a few suppliers, well
  below this run. The comments above each limit in
  [docker-compose.yaml](../../examples/docker-compose/docker-compose.yaml) say
  what to set at the report's scale, and
  [config.redis.example.conf](../../config.redis.example.conf) is the Redis
  configuration it ran with.
