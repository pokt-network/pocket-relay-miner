## Docker Compose HA reference

The smallest deployment that is still the HA architecture: one stateless
relayer, one miner and the shared Redis that holds every piece of session
state. It exists as **documentation** — the supported development environment
is the Tilt/Kubernetes localnet (`tilt/README.md`), which brings a chain,
backends and observability with it. This compose file brings none of that:
you point it at your own Pocket full node and your own service backends.

### Run it

```bash
cp ../../config.relayer.example.yaml config/relayer.yaml
cp ../../config.miner.example.yaml config/miner.yaml
$EDITOR config/relayer.yaml   # pocket_node endpoints, services/backends
$EDITOR config/miner.yaml     # pocket_node endpoints, chain id
$EDITOR config/supplier-keys.yaml  # your supplier keys — never commit this

docker compose up -d
docker compose logs -f relayer miner
```

Both configs must point `redis.url` at `redis://redis:6379` (the compose
service name) and `keys.keys_file` at `/keys/supplier-keys.yaml`.

### Scaling is the whole point

```bash
docker compose up -d --scale relayer=3 --scale miner=2
```

Nothing else changes: relayers are stateless, miners elect a leader through
Redis, and any replica can pick up another's in-flight work. When scaling
relayers, remove the fixed `ports:` mapping and front them with your own
load balancer.
