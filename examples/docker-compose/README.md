## Docker Compose example

A complete Pocket RelayMiner deployment on a local chain: one validator,
Redis, a test backend, one relayer and one miner. It serves real relays and
submits real claims and proofs, with nothing to install but Docker.

The step-by-step runbook, with the expected output of every step and what to
change for a real network, is [docs/deploy/DOCKER_COMPOSE.md](../../docs/deploy/DOCKER_COMPOSE.md).

Every key in `localnet/` and `config/supplier-keys.yaml` is a public localnet
key. Never use one on a real network.

### Files

- `docker-compose.yaml`: the services, their limits and their startup order.
- `config/relayer.yaml`, `config/miner.yaml`: minimal configs; every key not
  set takes its default (see `config.relayer.example.yaml` and
  `config.miner.example.yaml` at the repository root for all of them).
- `config/supplier-keys.yaml`: the signing keys of the 15 localnet suppliers.
- `localnet/`: the validator's genesis and node files, and `account-init.sh`,
  which puts the public key of every staked account on chain.
- Redis uses `config.redis.example.conf` from the repository root, with
  `maxmemory` lowered to 1 GB.

### Run it

The relayer and miner run `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0`.
If that tag is not published yet, build it from the repository root first:

```bash
docker build -t ghcr.io/pokt-network/pocket-relay-miner:v0.1.0 .
```

Then, from this directory:

```bash
docker compose -p prm-example up -d
docker compose -p prm-example ps -a
```

Expected: `validator`, `redis`, `backend`, `miner` and `relayer` are
`healthy`, and `account-init` is `Exited (0)`. The first run builds the test
backend image, which takes a few minutes.

Send a relay (from the repository root, after `make build`):

```bash
./bin/pocket-relay-miner relay jsonrpc --localnet --service develop-http
```

Expected: `Status: ✅ SUCCESS` and `Signature: ✅ VALID`. `--localnet` uses the
relayer on `localhost:8180` and the validator gRPC on `localhost:9090`, which
is where this compose file publishes them.

### Reset

```bash
docker compose -p prm-example down -v
```

`-v` deletes the chain and Redis together. Keep them together: a Redis that
holds sessions of a previous chain does not match a new one.
