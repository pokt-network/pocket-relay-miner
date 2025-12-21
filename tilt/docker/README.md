# Docker Compose Development Environment

This directory contains a Docker Compose setup for running the complete Pocket RelayMiner stack locally.

## Prerequisites

- Docker and Docker Compose v2+
- [Tilt](https://tilt.dev) (for hot-reload development)
- [hey](https://github.com/rakyll/hey) (for load testing): `go install github.com/rakyll/hey@latest`
- PATH image built and tagged as `path:latest`

## Quick Start

### Using Make (Recommended)

```bash
# From project root
make tilt-up-docker                    # Start environment
make tilt-up-docker ARGS="--stream"    # Start with log streaming
make tilt-down-docker                  # Stop environment
make tilt-down-docker ARGS="-v"        # Stop and remove volumes
```

### Using Tilt Directly

```bash
# From project root
tilt up -f tilt/docker/Tiltfile

# Or from this directory
cd tilt/docker && tilt up
```

### Using Docker Compose Only

```bash
cd tilt/docker
docker-compose up -d
docker-compose logs -f
docker-compose down -v
```

## Services

### Startup Order (Tiered Dependencies)

1. **Infrastructure**: redis, validator, backend, prometheus, grafana
2. **Initialization**: accounts-init (registers app pubkeys on-chain)
3. **Gateway/Miner**: path, miner
4. **Relayer**: relayer

| Service       | Ports                                             | Description                        |
|---------------|---------------------------------------------------|------------------------------------|
| Redis         | 6379                                              | Shared state and message broker    |
| Validator     | 26657 (RPC), 9090 (gRPC), 1317 (REST)             | Pocket Network local node          |
| Backend       | 8545 (HTTP), 50051 (gRPC), 9095 (Metrics)         | Demo backend server                |
| accounts-init | -                                                 | Registers app pubkeys (runs once)  |
| PATH          | 3069, 9096 (Metrics)                              | Gateway that routes relay requests |
| Relayer       | 8080, 8081 (Health), 9190 (Metrics), 6060 (pprof) | Stateless relay processor          |
| Miner         | 9092 (Metrics), 6065 (pprof)                      | Stateful claim/proof submitter     |
| Prometheus    | 9091                                              | Metrics collection                 |
| Grafana       | 3000                                              | Dashboards                         |

## Testing

### Send a Test Relay

```bash
# Wait for all services to be healthy
docker-compose ps

# Send a single test relay via PATH
curl -X POST http://localhost:3069/v1 \
  -H "Target-Service-Id: develop-http" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
```

### Load Test

```bash
# Send 3000 relays with 50 concurrent workers
hey -n 3000 -c 50 \
  -H "Target-Service-Id: develop-http" \
  -H "Content-Type: application/json" \
  -m POST \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  http://localhost:3069/v1
```

## Debugging

### Redis Commands

```bash
# Check Redis keys
go run ../../main.go redis keys --pattern "ha:*" --stats --redis redis://localhost:6379

# View sessions for a supplier
go run ../../main.go redis sessions --supplier pokt1... --redis redis://localhost:6379

# Check supplier distribution
go run ../../main.go redis supplier --claims --redis redis://localhost:6379
```

### Logs

```bash
# Follow all logs
docker-compose logs -f

# Follow specific service logs
docker-compose logs -f relayer miner
```

### Profiling

```bash
# CPU profile (30 seconds)
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Heap profile
go tool pprof http://localhost:6065/debug/pprof/heap

# Interactive web UI
go tool pprof -http=:8080 http://localhost:6060/debug/pprof/heap
```

## Observability Stack

Prometheus and Grafana are included by default:

- Prometheus: http://localhost:9091
- Grafana: http://localhost:3000 (admin/admin)

## Configuration

Configuration files are in `config/`:

- `relayer-config.yaml` - Relayer configuration
- `miner-config.yaml` - Miner configuration
- `path-config.yaml` - PATH gateway configuration
- `prometheus.yaml` - Prometheus scrape configuration
- `supplier-keys.yaml` - Supplier signing keys (hex private keys)

Shared configuration (genesis, validator) is in `../config/`:

- `genesis.json` - Pocket Network genesis
- `*.toml` - Validator configuration

## PATH Image

By default, the PATH gateway uses `ghcr.io/pokt-network/path:latest` from the GitHub Container Registry.

To use a custom image, set the `PATH_IMAGE` environment variable:

```bash
PATH_IMAGE=your-registry/path:tag docker-compose up -d
```

Or build locally:

```bash
# Clone PATH repository
git clone https://github.com/pokt-network/path.git
cd path

# Build and tag image
docker build -t path:latest .
PATH_IMAGE=path:latest docker-compose up -d
```

## Cleanup

```bash
# Stop services and remove volumes
docker-compose down -v

# Remove built images
docker rmi pocket-relay-miner:local path:latest
```
