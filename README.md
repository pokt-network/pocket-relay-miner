# Pocket RelayMiner

Serves relays for Pocket Network suppliers and gets them paid on chain.

## Start here

| I want to... | Read |
|---|---|
| deploy it with an AI agent, or read the rules first | [AGENTS.md](AGENTS.md) |
| choose how to deploy, and check the prerequisites | [docs/deploy/README.md](docs/deploy/README.md) |
| deploy with Docker Compose | [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) |
| deploy on a host (binary + systemd) | [docs/deploy/HOST.md](docs/deploy/HOST.md) |
| fix a deployment that does not start or does not serve | [docs/deploy/TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md) |
| configure the relayer, the miner or Redis | [Configuration](#configuration) |
| send test relays, or inspect Redis state | [CLI](#cli) |
| sign a relay from Node.js, Python or Rust | [examples/relay-signing/](examples/relay-signing/README.md) |
| contribute to this repository | [CONTRIBUTING.md](CONTRIBUTING.md) |

## What it is

1 binary, 2 processes, 1 Redis:

- **Relayer**: verifies each relay (ring signature, session, the application's
  remaining stake), forwards it to your backend, signs the response with the
  supplier's key and queues the relay in Redis. Transports: JSON-RPC over
  HTTP, WebSocket, gRPC, REST and streaming (SSE), CometBFT.
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

The supported topology is **1 relayer, 1 miner, 1 Redis**, with the relayer
and the miner on the same version.

## Deploy

| Path | Runbook |
|---|---|
| Docker Compose | [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md): a full local chain first, then your own node |
| Host (binary + systemd) | [docs/deploy/HOST.md](docs/deploy/HOST.md) |
| Kubernetes | not supported in v0.1.0 |

The image is `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0`. What a deployment
must hold (Redis 8.10 with `maxmemory` and `noeviction`, memory and CPU limits,
the miner needing the chain, the relayer waiting for the miner), and which of
those stop a binary at startup, is listed in [AGENTS.md](AGENTS.md#invariants).

## Configuration

- Relayer: [config.relayer.example.yaml](config.relayer.example.yaml), schema
  [config.relayer.schema.yaml](config.relayer.schema.yaml)
- Miner: [config.miner.example.yaml](config.miner.example.yaml), schema
  [config.miner.schema.yaml](config.miner.schema.yaml)
- Redis: [config.redis.example.conf](config.redis.example.conf)
- Minimal configs that start: [examples/docker-compose/config/](examples/docker-compose/config/)
  and [examples/host/](examples/host/)
- Supplier keys (keys file or keyring): [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md)

Validate a config before starting it, and check every staked service has a
backend:

```bash
pocket-relay-miner relayer validate --config relayer.yaml
pocket-relay-miner miner validate --config miner.yaml
pocket-relay-miner relayer validate --config relayer.yaml --check-stake
```

> **The relayer reads its config once, at startup.** There is no config
> watcher: `keys.hot_reload_enabled` governs **signing keys**, not the config
> file. Restart the process after every config change.

## CLI

```
pocket-relay-miner relayer   Start the relayer
pocket-relay-miner miner     Start the miner
pocket-relay-miner relay     Send test relays (single or load test)
pocket-relay-miner redis     Inspect Redis state
pocket-relay-miner version   Print the version
```

### Sending test relays

```bash
# 1 relay to the relayer of the local chain (see docs/deploy/DOCKER_COMPOSE.md)
pocket-relay-miner relay jsonrpc --localnet --service develop-http

# Load test, round-robin across all suppliers of the session
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --load-test --count 1000 --concurrency 50 --all-suppliers
```

Every transport, against any relayer: [docs/testing/DIRECT_CLI.md](docs/testing/DIRECT_CLI.md).

### Simulated relays

A simulated relay is signed with a **real ring signature** and served by the
**real backend**, but verified against a ring pinned in the relayer's config
instead of one read from chain: the relayer admits it without chain access,
and no application has to be staked. It is never metered and never published,
so it is never claimed or paid. Use it to test a live relayer end to end.

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --supplier <addr> --simulate --sim-key-id sim-http
```

Setup, per-transport key IDs, and how to verify nothing was charged:
[docs/SIMULATED_RELAYS.md](docs/SIMULATED_RELAYS.md).

### Inspecting Redis state

```bash
pocket-relay-miner redis --config miner.yaml leader                      # leader election
pocket-relay-miner redis --config miner.yaml sessions --supplier <addr>  # sessions
pocket-relay-miner redis --config miner.yaml submissions --supplier <addr>  # claims and proofs
pocket-relay-miner redis --config miner.yaml keys --stats                # keys, with type and TTL
```

## Signing a relay from another language

A relay is a signed request, so any language that can produce the signature
can send one. Relays are signed with a **bLSAG ring signature**, not a plain
secp256k1 signature. [examples/relay-signing/](examples/relay-signing/README.md)
documents the scheme byte for byte and ships working signers in Node.js,
Python and Rust, plus a Go oracle to check your own.

## Documentation

- [docs/deploy/](docs/deploy/README.md): deployment runbooks and troubleshooting
- [docs/SIMULATED_RELAYS.md](docs/SIMULATED_RELAYS.md): simulated relays
- [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md): supplier signing keys, keyring backends, passphrases, hot reload
- [docs/METRICS_TRIAGE.md](docs/METRICS_TRIAGE.md): reading the metrics
- [docs/REDIS.md](docs/REDIS.md): Redis architecture and key patterns
- [docs/CLAIM_PROOF_LIFECYCLE.md](docs/CLAIM_PROOF_LIFECYCLE.md): claim and proof windows
- [docs/CLAIM_LEAF_MODEL.md](docs/CLAIM_LEAF_MODEL.md): how relays become claim leaves
- [docs/PROTOCOL_SPEC.md](docs/PROTOCOL_SPEC.md): relay protocol
- [docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md](docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md): WebSocket handshake
- [docs/testing/](docs/testing/README.md): testing guides for contributors
- [CONTRIBUTING.md](CONTRIBUTING.md): development environment, tests and workflow

## License

MIT License - see [LICENSE](LICENSE)
