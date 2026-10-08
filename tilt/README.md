# Pocket RelayMiner - Tilt development environment

This directory holds the Tilt environment that runs a Pocket Network localnet
in a local kind cluster, with the relay miner under test in either mode:
`relay_miner_mode: ha` (the default; relayer and miner Deployments over Redis)
or `relay_miner_mode: standalone` (one process, its store on a volume, no Redis)
in `tilt_config.yaml`.

How to bring it up, what each mode runs, the port map, the smoke test, logs,
metrics and profiling are in [docs/testing/TILT.md](../docs/testing/TILT.md).
Sending relays and load is in [docs/testing/DIRECT_CLI.md](../docs/testing/DIRECT_CLI.md);
the live gate that validates either mode is in
[scripts/gates/README.md](../scripts/gates/README.md). This file only maps the
directory.

## Directory structure

```
Tiltfile                    # Entry point, at the repository root
tilt_config.example.yaml    # Tracked reference config; copy it to tilt_config.yaml
tilt/
├── k8s/                    # The Tiltfiles the entry point loads
│   ├── config.Tiltfile     # Config loading & validation
│   ├── defaults.Tiltfile   # Default values, relay_miner_mode among them
│   ├── ports.Tiltfile      # Centralized port registry
│   ├── utils.Tiltfile      # Helpers, the keyring init container all relay-miner pods share
│   ├── redis.Tiltfile      # Redis (high-availability mode, and the gateway)
│   ├── validator.Tiltfile  # Validator + genesis
│   ├── miner.Tiltfile      # Miner Deployment (high-availability mode)
│   ├── relayer.Tiltfile    # Relayer Deployment (high-availability mode)
│   ├── standalone.Tiltfile # Standalone Deployment (standalone mode)
│   ├── backend.Tiltfile    # Demo backend server
│   ├── nginx-backend.Tiltfile  # Static JSON-RPC backend for load tests
│   ├── observability.Tiltfile  # Prometheus, Grafana, Loki and Promtail
│   ├── path.Tiltfile       # The gateway that sends relays (optional)
│   ├── account-init.Tiltfile   # Account initialization
│   └── accounts.star       # Accounts account-init initializes, derived from the genesis
├── config/                 # Localnet chain files
├── backend-server/         # Demo backend server (its own Go module)
├── tiltcheck/              # Renders the Tiltfiles with Tilt stubbed, in the static gate (its own Go module)
├── local-registry.sh       # Local image registry for kind
└── README.md               # This file
```

## Demo backend (`backend-server/`)

The backend every localnet service is served from, one server for every
transport the relayer routes:

- HTTP JSON-RPC and WebSocket on `:8545`
- gRPC on `:50051`
- SSE streaming at `/stream/sse` and NDJSON streaming at `/stream/ndjson`

## Localnet chain files (`config/`)

| File | What it is |
|------|------------|
| `genesis.json` | the localnet genesis: 50 suppliers, the applications and the gateway, staked |
| `all-keys.yaml` | every localnet account's key; the supplier keys become the `supplier-keys` Secret |
| `config.toml`, `app.toml`, `client.toml` | the validator's configs; the Tiltfile sets the block time on `config.toml` |
| `node_key.json`, `priv_validator_key.json`, `priv_validator_state.json` | the validator's keys and state |

These are localnet values: the keys are public and fund nothing outside it.

## Grafana dashboards

Tilt provisions the 7 dashboards of
[examples/observability/](../examples/observability/README.md), the same files
the compose example runs; they are generated from the metrics the code defines
by `scripts/dashboards/generate.py`.
