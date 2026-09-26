## Docker Compose example

Redis, 1 relayer and 1 miner, pointed at the beta testnet
(`pocket-lego-testnet`) through the public Sauron endpoints. No chain runs
here: the node is remote. For a local chain to develop on, use Tilt
([docs/testing/TILT.md](../../docs/testing/TILT.md)).

The step-by-step runbook, with the expected output of every step, is
[docs/deploy/DOCKER_COMPOSE.md](../../docs/deploy/DOCKER_COMPOSE.md). It also
says what to switch for mainnet: every network-specific value in `config/` has
its mainnet value in a comment right above it, marked `Mainnet:`.

### Files

- `docker-compose.yaml`: the services, their limits and their startup order.
- `config/relayer.yaml`, `config/miner.yaml`: minimal configs; every key not
  set takes its default (see `config.relayer.example.yaml` and
  `config.miner.example.yaml` at the repository root for all of them).
  `config/relayer.yaml` declares 1 placeholder service, `my-service`, with a
  placeholder backend URL: replace both with yours.
- `config/supplier-keys.yaml`: 1 PUBLIC key that is not staked, so the stack
  starts as cloned and serves nothing. Never fund or stake it.
- `config/supplier-keys.local.yaml`: where YOUR keys go. It is gitignored
  (`.gitignore` here); point the 2 keys mounts in `docker-compose.yaml` at it.
- Redis uses `config.redis.example.conf` from the repository root, with
  `maxmemory` lowered to 1 GB. Its port is never published.

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

Expected: `redis`, `miner` and `relayer` are `healthy`. With the public key the
miner reports the supplier as not staked, and nothing is served until you put
your own keys and services in place (runbook steps 9 to 12).

### Reset

```bash
docker compose -p prm-example down -v
```

`-v` deletes the Redis data, including relays not yet claimed and claim trees
not yet proved.
