# Observability: Prometheus and Grafana

Seven Grafana dashboards, provisioned with Prometheus and a Redis exporter,
for the relayer and the miner. Every metric the binaries export is on a panel.
Open them in this order:

| Dashboard | The question it answers |
|---|---|
| 1 Money | Of what I served, how much is proved and paid, how much waits for the chain, how much is lost, and why? |
| 2 Claims and proofs | Is every claim and proof reaching the chain inside its window? |
| 3 Relay flow | Does every relay the relayer answered reach the tree the claim is built from? |
| 4 Relayer | Are gateways getting fast, accepted, billed answers, and if not, why? |
| 5 Storage and memory | Is Redis or process memory what is throttling me? |
| 6 Suppliers and chain | Stake, balance, keys, leadership and the chain clock |
| 7 Process internals | Where inside the binaries time and memory go |

A stat panel that is red is an alarm; each panel's description (the `i` next
to its title) says what it means and what to do. What to read first during an
incident is in [docs/METRICS_TRIAGE.md](../../docs/METRICS_TRIAGE.md).

## With the compose example

Start the compose example first
([docs/deploy/DOCKER_COMPOSE.md](../../docs/deploy/DOCKER_COMPOSE.md)), then,
from the repository root, add this file to the same project:

```bash
docker compose -p prm-example -f examples/docker-compose/docker-compose.yaml \
  -f examples/observability/docker-compose.observability.yaml up -d
curl -s http://127.0.0.1:9091/api/v1/targets | grep -o '"health":"[a-z]*"' | sort | uniq -c
```

**Expect**: `3 "health":"up"` (relayer, miner, Redis exporter). Then open
<http://127.0.0.1:3000> (user `admin`, password `admin`; change it on first
login): the Money dashboard is the home page, the others are in the
"Pocket RelayMiner" folder. Both ports are bound to loopback; from another
machine use an SSH tunnel (`ssh -L 3000:127.0.0.1:3000 <host>`). If 9091 or
3000 is taken, export `PROMETHEUS_PORT` or `GRAFANA_PORT` before `up`.

With the public, unstaked key the money panels stay at 0: nothing is served.
The chain panels (dashboard 6) and the process panels (dashboard 7) fill in at
once.

## With a host deployment

Run Prometheus and Grafana any way you like and:
- scrape the relayer's `metrics.addr` (127.0.0.1:9090 in `examples/host/`) as
  job `relayers`, the miner's (127.0.0.1:9092) as job `miners`, and a Redis
  exporter as job `redis`; set the label `instance` to `relayer` and `miner`,
  as [prometheus/prometheus.yml](prometheus/prometheus.yml) does;
- provision the JSON files in [grafana/dashboards/](grafana/dashboards/).

## Changing the dashboards

They are generated: edit `scripts/dashboards/dashboards_spec.py` and run
`python3 scripts/dashboards/generate.py`. `--check` fails when a metric the code
defines is on no panel, or when a JSON file differs from what the generator
writes.
