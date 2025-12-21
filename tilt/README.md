# Pocket RelayMiner - Tilt Development Environments

This directory contains Tilt-based development environments for Pocket RelayMiner (HA mode).

## Directory Structure

```
tilt/
├── k8s/                    # Kubernetes Tilt environment
│   ├── Tiltfile            # Main entry point for K8s
│   ├── config.Tiltfile     # Config loading & validation
│   ├── defaults.Tiltfile   # Default values
│   ├── ports.Tiltfile      # Centralized port registry
│   ├── utils.Tiltfile      # Helper functions
│   ├── redis.Tiltfile      # Redis deployment
│   ├── validator.Tiltfile  # Validator + genesis
│   ├── miner.Tiltfile      # Miner deployment
│   ├── relayer.Tiltfile    # Relayer deployment
│   ├── backend.Tiltfile    # Backend server
│   ├── observability.Tiltfile  # Prometheus + Grafana
│   ├── path.Tiltfile       # PATH gateway (optional)
│   └── account-init.Tiltfile   # Account initialization
├── docker/                 # Docker Compose environment
│   ├── Tiltfile            # Main entry point for Docker
│   ├── docker-compose.yaml # Docker Compose services
│   ├── config/             # Docker-specific configs
│   ├── scripts/            # Init scripts
│   └── README.md           # Docker-specific docs
├── config/                 # Shared configuration files
│   ├── genesis.json        # Pocket Network genesis
│   ├── all-keys.yaml       # All account keys
│   ├── supplier-keys.yaml  # Supplier signing keys
│   ├── *.toml              # Validator configs
│   └── *.json              # Validator keys
├── backend-server/         # Demo backend server
├── grafana/                # Grafana dashboards
│   ├── dashboards/         # JSON dashboard files
│   └── provisioning/       # Grafana provisioning
└── README.md               # This file
```

## Quick Start

### Option 1: Docker Compose (Simpler)

```bash
# From project root
make tilt-up-docker

# Or with streaming logs
make tilt-up-docker ARGS="--stream"

# Stop
make tilt-down-docker
make tilt-down-docker ARGS="-v"  # Remove volumes
```

### Option 2: Kubernetes (Production-like)

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

Configuration files used by both K8s and Docker environments:

| File | Description |
|------|-------------|
| `genesis.json` | Pocket Network genesis with apps, suppliers, gateway |
| `all-keys.yaml` | All account keys (apps, suppliers, gateway) |
| `supplier-keys.yaml` | Supplier signing keys for relayer/miner |
| `config.toml` | Validator CometBFT config |
| `app.toml` | Validator app config |

### Grafana Dashboards (`grafana/`)

Pre-configured dashboards for monitoring:
- Business Economics - Revenue and stake metrics
- Operational Health - System health and errors
- Service Performance - Latency and throughput

## Services

| Service | Docker Port | K8s Port | Description |
|---------|-------------|----------|-------------|
| Redis | 6379 | 6379 | Shared state |
| Validator RPC | 26657 | 26657 | Pocket node |
| Validator gRPC | 9090 | 9090 | Pocket queries |
| Backend HTTP | 8545 | 8545 | Demo backend |
| Backend gRPC | 50051 | 50051 | Demo backend |
| PATH Gateway | 3069 | 3069 | Relay routing |
| Relayer HTTP | 8080 | 8180+ | Relay processing |
| Miner Metrics | 9092 | 9092+ | Miner metrics |
| Prometheus | 9091 | 9091 | Metrics |
| Grafana | 3000 | 3000 | Dashboards |

## Testing Relays

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
# Docker Compose
docker-compose -f tilt/docker/docker-compose.yaml logs -f relayer

# Kubernetes
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
