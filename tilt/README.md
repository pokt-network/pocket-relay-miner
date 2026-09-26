# Pocket RelayMiner - Tilt Development Environments

This directory contains Tilt-based development environments for Pocket RelayMiner (HA mode).

## Directory Structure

```
Tiltfile                    # Entry point, at the repository root
tilt/
├── k8s/                    # Kubernetes Tilt environment
│   ├── config.Tiltfile     # Config loading & validation
│   ├── defaults.Tiltfile   # Default values
│   ├── ports.Tiltfile      # Centralized port registry
│   ├── utils.Tiltfile      # Helper functions
│   ├── redis.Tiltfile      # Redis deployment
│   ├── validator.Tiltfile  # Validator + genesis
│   ├── miner.Tiltfile      # Miner deployment
│   ├── relayer.Tiltfile    # Relayer deployment
│   ├── backend.Tiltfile    # Backend server
│   ├── nginx-backend.Tiltfile  # Static JSON-RPC backend for load tests
│   ├── observability.Tiltfile  # Prometheus, Grafana, Loki and Promtail
│   ├── path.Tiltfile       # PATH gateway (optional)
│   ├── account-init.Tiltfile   # Account initialization
│   └── accounts.star       # Accounts account-init initializes, derived from the genesis
├── config/                 # Shared configuration files
│   ├── genesis.json        # Pocket Network genesis
│   ├── all-keys.yaml       # All account keys
│   ├── *.toml              # Validator configs
│   ├── *.json              # Validator keys
│   └── scale/              # Genesis and keys sized for load (scripts/localnet/gen-genesis.go)
├── backend-server/         # Demo backend server
├── grafana/
│   └── dashboards/         # unified-overview.json
├── local-registry.sh       # Local image registry for kind
└── README.md               # This file
```

## Quick Start

```bash
# Prerequisites: kubectl, kind/minikube, tilt

# From project root
make tilt-up-k8s

# Stop
make tilt-down-k8s
```

## Components

### Backend Server (`backend-server/`)

Multi-protocol demo server for testing relay capabilities.

**Protocols Supported:**
- HTTP JSON-RPC (`:8545`)
- WebSocket subscriptions (`:8545`)
- gRPC (`:50051`)
- SSE streaming (`/stream/sse`)
- NDJSON streaming (`/stream/ndjson`)

### Shared Config (`config/`)

Configuration files for the K8s environment:

| File | Description |
|------|-------------|
| `genesis.json` | Pocket Network genesis with apps, suppliers, gateway |
| `all-keys.yaml` | All account keys (apps, suppliers, gateway) |
| `config.toml` | Validator CometBFT config |
| `app.toml` | Validator app config |

### Grafana Dashboard (`grafana/`)

One dashboard, `dashboards/unified-overview.json`: service and protocol
performance, claims, proofs, settlement, failed submissions and the cache.

## Services

| Service | Port | Description |
|---------|------|-------------|
| Redis | 6379 | Shared state |
| Validator RPC | 26657 | Pocket node |
| Validator gRPC | 9090 | Pocket queries |
| Backend HTTP | 8545 | Demo backend |
| Backend gRPC | 50051 | Demo backend |
| PATH Gateway | 3069 | Relay routing |
| Relayer HTTP | 8180 | Relay processing |
| Miner Metrics | 9092 | Miner metrics |
| Prometheus | 9091 | Metrics |
| Grafana | 3000 | Dashboards |

## Testing Relays

The check that counts is a relay sent straight to the relayer, which verifies
the signature and the backend's answer
([docs/testing/DIRECT_CLI.md](../docs/testing/DIRECT_CLI.md)):

```bash
# Expect: Status: ✅ SUCCESS
pocket-relay-miner relay jsonrpc --localnet --service develop-http
```

Through the gateway, which only confirms it is wired:

```bash
# Send a test relay via PATH
curl -X POST http://localhost:3069/v1 \
  -H "Target-Service-Id: develop-http" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'

# Expected response:
# {"id":1,"jsonrpc":"2.0","result":{"method":"eth_blockNumber","params":[],"status":"ok"}}
```

## Debugging

### Redis Commands

```bash
# Check Redis keys
go run main.go redis keys --pattern "ha:*" --stats

# View sessions
go run main.go redis sessions --supplier pokt1...

# Check leader status
go run main.go redis leader
```

### Logs

```bash
kubectl logs -f -l app=relayer
```

### Profiling

```bash
# Relayer pprof
go tool pprof http://localhost:6060/debug/pprof/profile

# Miner pprof
go tool pprof http://localhost:6065/debug/pprof/heap
```

## Architecture

### Request Flow

```
Client → PATH Gateway → Relayer → Backend
                 ↓
           Redis Streams
                 ↓
              Miner → Validator (claims/proofs)
```

### HA Failover

```
┌─────────────┐     ┌─────────────┐
│   Miner 1   │────▶│   Miner 2   │
│  (Leader)   │     │  (Standby)  │
└─────────────┘     └─────────────┘
       │                   │
       └───────┬───────────┘
               ↓
         Redis Lock
      (Leader Election)
```

## Performance Targets

- **Relayer**: 1000+ RPS per replica
- **Relay Validation**: <1ms average
- **SMST Update**: <100µs
- **Cache L1 Hit**: <100ns
- **Cache L2 Hit**: <2ms
