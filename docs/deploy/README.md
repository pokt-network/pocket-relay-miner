# Deploying Pocket RelayMiner v0.1.0

Start here, pick 1 path, and follow its runbook from the first step.

## Choose a path

| Path | Use it when | Runbook | Verified |
|---|---|---|---|
| Docker Compose | you want the fastest start, or a full local chain to try it on | [DOCKER_COMPOSE.md](DOCKER_COMPOSE.md) | end to end on a local chain: relay served, claim and proof on chain, reward settled |
| Host (binary + systemd) | you run services on VMs or bare metal without containers | [HOST.md](HOST.md) | configs and units checked; not verified end to end under systemd |
| Kubernetes | | not supported in v0.1.0 | |

The Tilt setup in `tilt/` is the developers' local environment. It is a
reference for how the pieces fit, not a supported deployment.

## How the pieces fit

```
  gateways ──> relayer :8080 ──> your backends
                  │  │
                  │  └──── reads the chain (applications, sessions, params)
                  ▼
                Redis  (relays, claim trees, stake meter, all shared state)
                  ▲
                  │
                miner ────> the chain (claims, proofs)
```

- **Relayer**: verifies each relay (ring signature, session, the application's
  remaining stake), forwards it to the backend of its service, signs the
  response with the supplier's key and queues the relay in Redis.
- **Miner**: reads the queued relays, builds 1 claim tree per session and
  supplier, and submits the claim and then the proof in their on-chain windows.
- **Redis**: holds all shared state. It is not a cache: a lost key is a claim
  that cannot be proved.

**Topology: 1 relayer, 1 miner, 1 Redis.** v0.1.0 was tested and measured on
this topology only.

## Invariants

Each row says what breaking it does: some stop a binary at startup, some make
it serve nothing, and some are only unsupported. Each links to the error it
produces, where there is one.

| # | Invariant | If broken |
|---|---|---|
| 1 | Relayer and miner run the same version: `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0`, or binaries built from tag `v0.1.0` | mixed versions are not supported |
| 2 | Redis with `maxmemory` set and `maxmemory-policy noeviction` ([config.redis.example.conf](../../config.redis.example.conf)). 8.10 is the supported version; the version is not checked at startup | `maxmemory` 0 or another policy: [both refuse to start](TROUBLESHOOTING.md#redis) |
| 3 | `GOMEMLIMIT` and `GOMAXPROCS` set, or container / systemd memory and CPU limits | [each process sizes itself from the whole host](TROUBLESHOOTING.md#memory-and-cpu) |
| 4 | Miner config has `block_time_seconds` and the right `pocket_node.chain_id`, and the node is reachable | [the miner exits](TROUBLESHOOTING.md#miner-does-not-start) |
| 5 | The miner runs before relays are expected: the relayer's `/ready` is 503 until the miner publishes its service factor manifest | [relayer up, every relay refused](TROUBLESHOOTING.md#relayer-up-but-not-ready) |
| 6 | `relayer validate` and `miner validate` exit 0 on the exact config files you start | [config rejected](TROUBLESHOOTING.md#config-rejected) |

## Prerequisites

| You need | For | Where it comes from |
|---|---|---|
| A Pocket full node: CometBFT RPC and gRPC | both processes; the miner submits transactions through it | yours, or a provider's. The compose runbook runs a local one for you |
| At least 1 staked supplier and its private key (64 hex characters) | signing responses, claims and proofs | your staking process. **A human provides it; an agent never generates or moves funds** |
| A backend node for every service your suppliers are staked for | answering relays | yours |
| Redis 8.10 | shared state | [config.redis.example.conf](../../config.redis.example.conf) |
| The supplier's account funded for transaction fees | claims and proofs cost fees | your wallet |

The miner's `block_time_seconds` must match the network: mainnet is roughly
60 seconds; measure yours.

## Ports

| Process | Port | Serves |
|---|---|---|
| relayer | 8080 | relay traffic (`listen_addr`) |
| relayer | 8081 | `GET /health` (always 200 while running), `GET /ready` (200 only when it can serve) |
| relayer | 9090 | Prometheus metrics (`metrics.addr`) |
| relayer | 6060 | pprof profiling (`pprof.addr`). The code default is `0.0.0.0:6060`; both example configs set `127.0.0.1:6060`. Never expose it |
| miner | 9092 | Prometheus metrics and `GET /health` (`metrics.addr`) |
| Redis | 6379 | never expose it outside the host or the compose network |

Relayer metrics on 9090 and a node's gRPC on 9090 collide when both run on the
same host network. The host runbook binds metrics and pprof to `127.0.0.1`; move one of
them if your node is on the same host.

## Startup order

1. Redis, and check `maxmemory-policy` is `noeviction`.
2. Validate both configs: exit 0.
3. Miner. It exits within about 10 seconds if it cannot read the chain.
4. Relayer. Its `/ready` turns 200 once the miner's manifest is in Redis.
5. Send a relay, then watch the claim land after the session ends.

## Upgrading and rolling back

- Upgrade the relayer and the miner together.
- Validate the new configs with the new binary before switching: retired keys
  fail `validate`.
- v0.1.0 is the first release, so there is no earlier release to roll back to.
  Builds from before it cannot read the compacted trees: do not switch to one
  while claimed sessions still await their proof.
