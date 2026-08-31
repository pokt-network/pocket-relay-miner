# Pocket RelayMiner (High Availability)

Production-grade, horizontally scalable relay mining service for Pocket Network.

## Features

- **Multi-Transport Support**: JSON-RPC (HTTP), WebSocket, gRPC, REST/Streaming (SSE)
- **Horizontal Scaling**: Stateless relayers scale independently behind load balancers
- **High Availability**: Redis-backed shared state with automatic leader election
- **Relay Validation**: Ring signature verification, session validation, supplier signing
- **Relay Metering**: Rate limiting based on application stake
- **Simulated Relays**: Exercise a live relayer end-to-end — real signature, real backend — without minting a claimable relay ([guide](docs/simulated-relays.md))
- **Observability**: Prometheus metrics, pprof profiling, structured logging

## Architecture

```
                  ┌─────────────────┐
                  │  Load Balancer  │
                  └────────┬────────┘
           ┌───────────────┼───────────────┐
           │               │               │
     ┌─────┴─────┐   ┌─────┴─────┐   ┌─────┴─────┐
     │ Relayer 1 │   │ Relayer 2 │   │ Relayer N │  (stateless, scales horizontally)
     └─────┬─────┘   └─────┬─────┘   └─────┬─────┘
           └───────────────┼───────────────┘
                           │
                    ┌──────┴──────┐
                    │    Redis    │  (shared state)
                    └──────┬──────┘
                           │
              ┌────────────┴────────────┐
              │                         │
        ┌─────┴─────┐             ┌─────┴─────┐
        │   Miner   │             │   Miner   │  (leader election)
        │ (Leader)  │             │ (Standby) │
        └───────────┘             └───────────┘
```

**Relayer**: Validates relay requests, signs responses, publishes to Redis Streams
**Miner**: Consumes relays, builds SMST trees, submits claims/proofs to blockchain

## Requirements

- Go 1.26.5+ (matches `go.mod` and CI)
- Redis 8.2+ (required for XACKDEL command)
- Access to Pocket Network Shannon endpoints

## Quick Start

### Build

```bash
make build          # Development build
make build-release  # Optimized release build
```

### Run

```bash
# Start miner (claim/proof submission)
pocket-relay-miner miner --config config.miner.yaml

# Start relayer (relay proxy)
pocket-relay-miner relayer --config config.relayer.yaml
```

## Configuration

Example configurations with full documentation:

- **Relayer**: [`config.relayer.example.yaml`](config.relayer.example.yaml)
- **Miner**: [`config.miner.example.yaml`](config.miner.example.yaml)
- **Schema**: [`config.relayer.schema.yaml`](config.relayer.schema.yaml), [`config.miner.schema.yaml`](config.miner.schema.yaml)

Validate a config before deploying it, and cross-check it against on-chain stake:

```bash
pocket-relay-miner relayer validate --config config.relayer.yaml
pocket-relay-miner relayer validate --config config.relayer.yaml --check-stake
```

> **The relayer reads its config once, at startup.** There is no config watcher:
> `hot_reload_enabled` appears in both schemas, but it governs **signing keys**,
> not the config file. A deployment that mounts the config from a Kubernetes
> ConfigMap must therefore restart the pods on every config change — otherwise
> the ConfigMap updates, the GitOps controller reports Synced and Healthy, and
> the process keeps serving the old config.

## Local Development

This project uses [Tilt](https://tilt.dev/) for local development with two environment options.

### Kubernetes (Recommended)

```bash
# Start Kubernetes dev environment (requires kind cluster)
make tilt-up-k8s

# Stop environment
make tilt-down-k8s
```

### Docker Compose

For environments without Kubernetes:

For a Tilt-free single-replica HA reference (documentation-oriented, not a
dev loop), see `examples/docker-compose/`.

### Access Services

When running either environment:
- PATH Gateway: `localhost:3069`
- Relayer: `localhost:8180`
- Prometheus: `localhost:9091`
- Grafana: `localhost:3000`

See [`tilt/README.md`](tilt/README.md) for detailed setup instructions.

## CLI Commands

```bash
pocket-relay-miner <command>

Commands:
  relayer       Start the relayer service
  miner         Start the miner service
  relay         Test relay requests (supports load testing)
  redis         Debug Redis state and HA components
  version       Display version information
```

### Testing Relays

```bash
# Single relay test (direct to a relayer replica)
pocket-relay-miner relay jsonrpc --localnet --service develop-http

# Load test, round-robin across all session suppliers
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --load-test --count 1000 --concurrency 50 --all-suppliers
```

Full testing guides — Tilt bring-up, PATH+`hey` load, and direct-CLI testing of
all five transports (JSON-RPC, WebSocket, gRPC, streaming, CometBFT) — are in
[`docs/testing/`](docs/testing/README.md).

### Simulated Relays

A simulated relay is signed with a **real ring signature** and served by the
**real backend**, but it is verified against a ring pinned in the relayer's
config instead of one read from chain — so **the relayer admits it without any
chain access**, and no application has to be staked. It is never metered and
never published, so it never becomes part of a claim and is never paid for. Use
it to exercise a live relayer end to end.

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --supplier <addr> --simulate --sim-key-id sim-http
```

See [`docs/simulated-relays.md`](docs/simulated-relays.md) for configuration, the
per-transport key IDs, and how to verify that nothing was charged.

### Debugging Redis State

```bash
pocket-relay-miner redis leader              # Check leader election
pocket-relay-miner redis sessions --supplier <addr>  # Inspect sessions
pocket-relay-miner redis smst --session <id>  # View SMST tree
pocket-relay-miner redis keys --pattern "ha:*" --stats  # List all keys
```

## Signing a Relay from Another Language

The CLI above is convenient, but nothing requires it: a relay is just a signed
request, so any language that can produce the signature can send one. This
matters if you are building a gateway, or health-check tooling, outside Go.

Relays are signed with a **bLSAG ring signature**, not a plain secp256k1
signature — and the scheme has no specification other than the behaviour of the
Go libraries that implement it.
[`examples/relay-signing/`](examples/relay-signing/README.md) documents it
byte-for-byte and ships working, verified signers in **Node.js**, **Python** and
**Rust**, plus a Go **oracle** that checks an implementation of your own against
the same `ring-go` the relayer runs.

Two things worth knowing before you start:

- **You only need one private key.** The ring is `[application, gateway]`, but it
  is built from **public** keys and signed by a single private one — the
  signer's, normally the gateway's. That is what delegation means: a gateway
  signs for an application without ever holding its key. The keys themselves are
  plain secp256k1 scalars, 32 bytes of hex — no keyring, no armor, no derivation.
- **Signing a real relay and a simulated one is the same act.** So the examples
  use the simulated path to prove a signer works end-to-end against a real
  relayer, without staking anything or touching a chain.

## Development

### Testing

```bash
make test              # Run all tests
make test_miner        # Run miner tests with race detection
make test-coverage     # Generate coverage report
```

**Test Quality Standards** (Rule #1 - Cannot be broken):
- ✅ All tests use real miniredis (no mocks)
- ✅ All tests pass with `-race` flag (no race conditions)
- ✅ All tests are deterministic (no flaky behavior)
- ✅ No arbitrary timeouts or sleeps

### Code Quality

```bash
make fmt            # Format code
make lint           # Run linters
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) for development workflow and guidelines.

## Documentation

- [`docs/testing/`](docs/testing/README.md) - Testing guides (Tilt bring-up, PATH+hey load, direct-CLI per-protocol)
- [`docs/simulated-relays.md`](docs/simulated-relays.md) - Simulated relays: what they are, how to enable and fire them, and how to verify nothing was charged
- [`examples/relay-signing/`](examples/relay-signing/README.md) - Signing a relay in Node.js, Python or Rust: the ring-signature scheme byte-for-byte, working signers, and an oracle to verify your own
- [`docs/SUPPLIER_KEYS.md`](docs/SUPPLIER_KEYS.md) - Supplier signing keys: the two sources, the keyring
  backends and why only two are supported, where the passphrase comes from, and how hot reload behaves
- [`docs/PROTOCOL_SPEC.md`](docs/PROTOCOL_SPEC.md) - Relay protocol specification
- [`docs/REDIS.md`](docs/REDIS.md) - Redis architecture and key patterns
- [`docs/CLAIM_PROOF_LIFECYCLE.md`](docs/CLAIM_PROOF_LIFECYCLE.md) - Claim/proof windows and inclusion reconciler
- [`docs/CLAIM_LEAF_MODEL.md`](docs/CLAIM_LEAF_MODEL.md) - How relays become claim leaves and what a claim commits to
- [`docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md`](docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md) - WebSocket protocol details
- [`CLAUDE.md`](CLAUDE.md) - Technical reference for contributors
- [`CONTRIBUTING.md`](CONTRIBUTING.md) - Contribution guidelines
- [`tilt/README.md`](tilt/README.md) - Local development setup

## License

MIT License - see [LICENSE](LICENSE)
