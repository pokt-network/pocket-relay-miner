# Pocket RelayMiner

Serves relays for Pocket Network suppliers and gets them paid on chain.

## What it is

1 binary, 2 processes, 1 Redis:

- **Relayer**: verifies each relay (ring signature, session, the application's
  remaining stake), forwards it to your backend, signs the response with the
  supplier's key and queues the relay in Redis.
- **Miner**: builds the claim tree of each session from the queued relays and
  submits the claim and the proof to the chain.
- **Redis**: all shared state: relays, claim trees, the stake meter.

```
  gateways ──> relayer ──> your backends
                  │
                  ▼
                Redis
                  ▲
                  │
                miner ──> Pocket chain (claims, proofs)
```

A deployment is **1 Redis shared by the relayers and miners**, all on the same
version. v0.1.0 was tested on 1 relayer + 1 miner and on 2 relayers + 2 miners
(the latter at lower load); the load tests are from 1 relayer + 1 miner.

## What it does

1. **Serves relays over every transport**: JSON-RPC over HTTP, WebSocket, gRPC,
   REST and streaming (SSE), CometBFT, routed to your backends per service.
2. **Charges each relay against the application's stake** and refuses what the
   session can no longer pay for. If Redis cannot confirm the budget, the relay
   is refused rather than served for free.
3. **Claims and proves every session automatically**, and keeps watching until
   each claim and proof is included on chain; what did not land is resubmitted
   while its window is open. See [docs/CLAIM_PROOF_LIFECYCLE.md](docs/CLAIM_PROOF_LIFECYCLE.md).
4. **Runs many suppliers in one process**, with keys from a keys file or a
   keyring, reloaded without a restart. See [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md).
5. **Fails over between miners**: one leader at a time, elected through Redis.
6. **Refuses an unsafe setup before serving**: `validate` checks a config
   offline, `--check-stake` checks every staked service has a backend, and both
   processes refuse to start on a Redis that could lose data.
7. **Sends test and load relays** on every transport, including **simulated
   relays** that exercise a live relayer without staking or billing anything.
   See [docs/SIMULATED_RELAYS.md](docs/SIMULATED_RELAYS.md).
8. **Shows its state**: `pocket-relay-miner redis` decodes sessions, streams,
   claim trees, meters and submissions, and the metrics have a triage order
   ([docs/METRICS_TRIAGE.md](docs/METRICS_TRIAGE.md)).
9. **Documents relay signing byte for byte**, with working signers in Node.js,
   Python and Rust: [examples/relay-signing/](examples/relay-signing/README.md).

## Where to start

**Deploy.** An AI agent starts at [AGENTS.md](AGENTS.md): the rules and the
invariants that stop a deployment.

| I want to... | Read |
|---|---|
| choose a path, check prerequisites and ports | [docs/deploy/README.md](docs/deploy/README.md) |
| run it with Docker Compose, a whole local chain first | [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) |
| run it on a host, binary and systemd | [docs/deploy/HOST.md](docs/deploy/HOST.md) |
| use Kubernetes | no example in v0.1.0; `tilt/` runs the stack on a local kind cluster and is a starting point for your own manifests |

**Configure.** The relayer reads its config once, at startup: restart it after
every change.

| I want to... | Read |
|---|---|
| see every key, its default and why to change it | [config.relayer.example.yaml](config.relayer.example.yaml), [config.miner.example.yaml](config.miner.example.yaml) |
| check a config against its schema | [config.relayer.schema.yaml](config.relayer.schema.yaml), [config.miner.schema.yaml](config.miner.schema.yaml), and `pocket-relay-miner relayer\|miner validate --config <file>` |
| set up Redis | [config.redis.example.conf](config.redis.example.conf) |
| set up supplier keys | [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md) |
| start from a minimal config that works | [examples/docker-compose/config/](examples/docker-compose/config/), [examples/host/](examples/host/) |

**Operate.**

| I want to... | Read |
|---|---|
| fix a deployment that does not start or does not serve | [docs/deploy/TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md) |
| know which metrics to read, in order | [docs/METRICS_TRIAGE.md](docs/METRICS_TRIAGE.md), and [scripts/observability/triage.sh](scripts/observability/triage.sh) to check them against Prometheus |
| inspect what is in Redis | `pocket-relay-miner redis --help`; what each key holds: [docs/REDIS.md](docs/REDIS.md) |

**Test and measure.**

| I want to... | Read |
|---|---|
| send a relay or a load test to a relayer, on any transport | [docs/testing/DIRECT_CLI.md](docs/testing/DIRECT_CLI.md) |
| test a live relayer without staking or billing | [docs/SIMULATED_RELAYS.md](docs/SIMULATED_RELAYS.md) |
| size the connection pool for each backend | [scripts/loadtest/README.md](scripts/loadtest/README.md) |
| sign a relay from another language | [examples/relay-signing/](examples/relay-signing/README.md) |

**Understand.**

| I want to... | Read |
|---|---|
| follow a relay into a claim, a proof and a reward | [docs/CLAIM_PROOF_LIFECYCLE.md](docs/CLAIM_PROOF_LIFECYCLE.md) |
| know when two relays count as one | [docs/CLAIM_LEAF_MODEL.md](docs/CLAIM_LEAF_MODEL.md) |
| learn the protocol per entity, and where the money moves | [docs/protocol/](docs/protocol/README.md) |
| know what a gateway, the relayer and a backend expect of each other | [docs/PROTOCOL_SPEC.md](docs/PROTOCOL_SPEC.md), [docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md](docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md) |

**Develop.** [CONTRIBUTING.md](CONTRIBUTING.md): the package map, the
development environment, the workflow and every rule for changing the code.
Testing guides: [docs/testing/](docs/testing/README.md) and
[scripts/README.md](scripts/README.md).

## License

MIT License - see [LICENSE](LICENSE)
