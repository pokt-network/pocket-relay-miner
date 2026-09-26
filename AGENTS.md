# AGENTS.md

Read this first. It routes you to the right document and lists what stops a
deployment from starting.

## What this is

Pocket RelayMiner serves relays for Pocket Network suppliers and gets them paid.
It is 2 processes from 1 binary, sharing 1 Redis: the **relayer** verifies each
relay, forwards it to your backend and signs the response; the **miner** builds
the claim and the proof from the served relays and submits them to the chain.

## Index

**Deploy**

| Document | What it is for |
|---|---|
| [docs/deploy/README.md](docs/deploy/README.md) | choose a path; prerequisites, ports and how the pieces fit |
| [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) | runbook: a whole local chain first, then your own node |
| [docs/deploy/HOST.md](docs/deploy/HOST.md) | runbook: binary and systemd on a host |
| [examples/docker-compose/](examples/docker-compose/README.md), [examples/host/](examples/host) | the files those runbooks use |

**Configure**

| Document | What it is for |
|---|---|
| [config.relayer.example.yaml](config.relayer.example.yaml), [config.miner.example.yaml](config.miner.example.yaml) | every key, with its default and why you would change it |
| [config.relayer.schema.yaml](config.relayer.schema.yaml), [config.miner.schema.yaml](config.miner.schema.yaml) | the schemas `validate` checks against |
| [config.redis.example.conf](config.redis.example.conf) | the Redis settings the release was measured with |
| [docs/SUPPLIER_KEYS.md](docs/SUPPLIER_KEYS.md) | supplier keys: keys file, keyring, hot reload |

**Operate and diagnose**

| Document | What it is for |
|---|---|
| [docs/deploy/TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md) | a symptom, its cause and the fix |
| [docs/METRICS_TRIAGE.md](docs/METRICS_TRIAGE.md) | which metrics to read after a run or during an incident, in order |
| [docs/REDIS.md](docs/REDIS.md) | what lives in Redis and why losing it costs money |
| `pocket-relay-miner redis --help` | inspect sessions, streams, caches, meters and submissions in Redis |
| [scripts/observability/triage.sh](scripts/observability/triage.sh) | checks the identities in METRICS_TRIAGE.md against Prometheus and prints OK or GAP per line (copy `triage.conf.example` first) |

**Understand what it does**

| Document | What it is for |
|---|---|
| [docs/CLAIM_PROOF_LIFECYCLE.md](docs/CLAIM_PROOF_LIFECYCLE.md) | how served relays become claims, proofs and rewards |
| [docs/CLAIM_LEAF_MODEL.md](docs/CLAIM_LEAF_MODEL.md) | when two relays collapse into one tree leaf |
| [docs/protocol/](docs/protocol/README.md) | the protocol per entity (application, gateway, supplier, session, service, params) and where the money moves |
| [docs/PROTOCOL_SPEC.md](docs/PROTOCOL_SPEC.md), [docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md](docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md) | what a gateway/client, the relayer and a backend expect from each other |
| [examples/relay-signing/](examples/relay-signing/README.md) | how to sign and send a relay from other languages |

**Test and measure**

| Document | What it is for |
|---|---|
| [docs/SIMULATED_RELAYS.md](docs/SIMULATED_RELAYS.md) | exercise a running relayer end to end with the `relay` command |
| [scripts/loadtest/README.md](scripts/loadtest/README.md) | measure each backend's RPS ceiling and size its connection pool |
| [docs/testing/](docs/testing/README.md), [scripts/README.md](scripts/README.md) | the development environment and its test scripts |

**Develop**: [CONTRIBUTING.md](CONTRIBUTING.md) holds every rule for changing
the code.

v0.1.0 ships no Kubernetes example or runbook. The Tilt setup in `tilt/` runs
the relayer, the miner and Redis on a local kind cluster for development: it is
a starting point for your own manifests, not a production config. The
invariants below hold on any platform.

## Invariants

Some of these are checked at startup and stop a binary when broken; the rest
are not checked, and breaking them is unsupported.

1. **Topology: 1 Redis shared by the relayers and miners.** v0.1.0 was tested
   on 1 relayer + 1 miner and on 2 relayers + 2 miners, the latter at lower
   load; the load tests and every capacity figure are from 1 relayer + 1 miner.
2. **Same version for relayer and miner**: image
   `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0` for both, or the binary of
   the same tag. Mixed versions are not supported. Never use a moving tag.
3. **Redis 8.10 or newer, with `maxmemory` set and `maxmemory-policy
   noeviction`.** Both binaries refuse to start when `redis_version` is below
   8.10, when `maxmemory` is 0, or when the policy is anything other than
   `noeviction`. `validate` does not connect to Redis: these are checked when
   the process starts. Use
   [config.redis.example.conf](config.redis.example.conf).
4. **Memory and CPU limits**: set `GOMEMLIMIT` and `GOMAXPROCS`, or give each
   process a container (or systemd) memory and CPU limit. Without either, each
   process sizes itself from the whole host.
5. **The miner needs the chain at startup.** Set `block_time_seconds` and
   `pocket_node.chain_id` in the miner config; the miner exits if the node is
   unreachable or reports another network.
6. **The relayer's `/ready` answers 503 until the miner has published its
   service factor manifest.** Start the miner first. A relayer that is up with
   `/ready` at 503 is waiting for the miner, not broken.
7. **Validate before starting**: `pocket-relay-miner relayer validate --config <file>`
   and `pocket-relay-miner miner validate --config <file>` must exit 0.
   `validate` does not contact Redis or the chain, so exit 0 is necessary, not
   sufficient.

## Rules for an agent running a deployment

- Pick 1 runbook, [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md)
  or [docs/deploy/HOST.md](docs/deploy/HOST.md), and run its steps in order
  from Step 0. A step gives **Run** and **Expect**, and **If not** and
  **Stop if** where they apply. Compare the output with **Expect** before going on.
- **Stop and ask a human** before using real supplier keys, spending funds,
  staking, or pointing anything at mainnet. The keys in
  `examples/docker-compose/` are public localnet keys: never use them on a real
  network.
