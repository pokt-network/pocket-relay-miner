# AGENTS.md

Read this first. It routes you to the right document and lists what stops a
deployment from starting.

## What this is

Pocket RelayMiner serves relays for Pocket Network suppliers and gets them paid.
It is 2 processes from 1 binary, sharing 1 Redis: the **relayer** verifies each
relay, forwards it to your backend and signs the response; the **miner** builds
the claim and the proof from the served relays and submits them to the chain.

## Where to go

| I want to... | Read |
|---|---|
| deploy with docker compose | [docs/deploy/DOCKER_COMPOSE.md](docs/deploy/DOCKER_COMPOSE.md) |
| deploy on a host (binary + systemd) | [docs/deploy/HOST.md](docs/deploy/HOST.md) |
| choose a path, or check the prerequisites | [docs/deploy/README.md](docs/deploy/README.md) |
| configure the relayer or the miner | [config.relayer.example.yaml](config.relayer.example.yaml), [config.miner.example.yaml](config.miner.example.yaml), [config.redis.example.conf](config.redis.example.conf) |
| troubleshoot a deployment | [docs/deploy/TROUBLESHOOTING.md](docs/deploy/TROUBLESHOOTING.md) |
| develop this repository | [CONTRIBUTING.md](CONTRIBUTING.md), then [CLAUDE.md](CLAUDE.md) |

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
