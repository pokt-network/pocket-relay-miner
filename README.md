# Pocket RelayMiner (High Availability)

Production-grade, horizontally scalable relay mining service for Pocket Network.

## Features

- **Multi-Transport Support**: JSON-RPC (HTTP), WebSocket, gRPC, REST/Streaming (SSE)
- **Horizontal Scaling**: Stateless relayers scale independently behind load balancers
- **High Availability**: Redis-backed shared state with automatic leader election
- **Relay Validation**: Ring signature verification, session validation, supplier signing
- **Relay Metering**: Rate limiting based on application stake
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

- Go 1.24.3+
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

```bash
# Start Docker Compose dev environment
make tilt-up-docker

# Stop environment
make tilt-down-docker
```

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
# Single relay test
pocket-relay-miner relay jsonrpc --localnet --service develop-http

# Load test
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --load-test --count 1000 --concurrency 50
```

### Debugging Redis State

```bash
pocket-relay-miner redis leader              # Check leader election
pocket-relay-miner redis sessions --supplier <addr>  # Inspect sessions
pocket-relay-miner redis smst --session <id>  # View SMST tree
pocket-relay-miner redis keys --pattern "ha:*" --stats  # List all keys
```

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

- [`docs/PROTOCOL_SPEC.md`](docs/PROTOCOL_SPEC.md) - Relay protocol specification
- [`docs/REDIS.md`](docs/REDIS.md) - Redis architecture and key patterns
- [`docs/TESTING.md`](docs/TESTING.md) - Testing guide
- [`docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md`](docs/WEBSOCKET_HANDSHAKE_PROTOCOL.md) - WebSocket protocol details
- [`CLAUDE.md`](CLAUDE.md) - Technical reference for contributors
- [`CONTRIBUTING.md`](CONTRIBUTING.md) - Contribution guidelines
- [`tilt/README.md`](tilt/README.md) - Local development setup

## License

MIT License - see [LICENSE](LICENSE)
