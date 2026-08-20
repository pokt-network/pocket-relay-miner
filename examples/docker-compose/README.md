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
cp config/supplier-keys.yaml.example config/supplier-keys.yaml
```

Then edit the copies — every step is required:

1. **Set `redis.url: redis://redis:6379` in BOTH `config/relayer.yaml` and
   `config/miner.yaml`.** The example configs dial `redis://localhost:6379`,
   which inside a container is the container itself — both services
   crash-loop until this points at the compose service name.
2. Set `keys.keys_file: /keys/supplier-keys.yaml` in both configs (the path
   the compose file mounts the keys at).
3. In `config/relayer.yaml`: point `pocket_node` at your full node and
   configure your services/backends.
4. In `config/miner.yaml`: point `pocket_node` at your full node and set the
   chain id.
5. In `config/supplier-keys.yaml`: replace the placeholder with your real
   hex-encoded supplier private keys — **never commit this file**.

```bash
docker compose up -d
docker compose logs -f relayer miner
```

### Scaling is the whole point

```bash
docker compose up -d --scale relayer=3 --scale miner=2
```

Nothing else changes: relayers are stateless, miners elect a leader through
Redis, and any replica can pick up another's in-flight work. When scaling
relayers, remove the fixed `ports:` mapping and front them with your own
load balancer.
