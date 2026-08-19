# Testing in Tilt (localnet bring-up)

This is the zero-to-running guide for the local test environment. Tilt spins up
a full Pocket Network localnet in a **kind** Kubernetes cluster (context
`kind-kind`): a validator, Redis, the relayer + miner under test, demo backends,
a PATH gateway, and an observability stack. Once it is up you send relays with
the `relay` CLI straight at a relayer ([DIRECT_CLI.md](DIRECT_CLI.md)) and run
the HA/chaos suite against it.

This doc is one of two testing guides; the index is
[README.md](README.md).

## 1. Bring up the localnet

Prerequisites: `kubectl`, `kind` (or minikube), `tilt`, and `helm` (the Redis
operator is installed via Helm). Tilt builds every image inside the cluster —
you do **not** run `make build` or `go build` first.

```bash
# From the project root: start the Kubernetes (kind) environment
make tilt-up-k8s
```

```bash
# Stream Tilt logs to the terminal instead of just the UI
make tilt-up-k8s ARGS="--stream"
```

```bash
# Tear everything down
make tilt-down-k8s
```

Tilt watches the source tree and **rebuilds/redeploys automatically** on file
change — never manually build, `kubectl delete pod`, or `kubectl port-forward`;
Tilt owns all of that. Open the Tilt UI at <http://localhost:10350> to watch
resources come up.

There is no Docker-Compose dev variant: the K8s environment above is the
production-like one and the target for the HA/chaos scripts. A minimal
compose file for documentation purposes lives in `examples/docker-compose/`.

## 2. What you get (pods & replicas)

With the default `tilt_config.yaml` (PATH and observability both enabled) the
cluster brings up:

| Resource | Kind | Replicas | Notes |
|---|---|---|---|
| `relayer` | Deployment | **2** | stateless multi-transport proxy (under test) |
| `miner` | Deployment | **2** | stateful claim/proof, leader-elected |
| `path` | Deployment | 1 | PATH gateway, centralized mode |
| `validator` | Deployment | 1 | `pocketd` Shannon node (chain-id `pocket`) |
| `redis` | StatefulSet | 1 | pod `redis-standalone-0` (via Redis operator) |
| `redis-operator` | Deployment | 1 | Helm-installed operator |
| `backend` / `backend-2` | Deployment | 1 each | demo RPC backends (multi-backend pool) |
| `nginx-backend` | Deployment | 1 | nginx backend for pool testing |
| `account-init` | Job | — | one-shot: funds the localnet accounts (apps, suppliers, gateway) |
| `prometheus` | Deployment | 1 | metrics |
| `grafana` | Deployment | 1 | dashboards |
| `loki` | Deployment | 1 | log aggregation |
| `promtail` | DaemonSet | 1/node | ships pod logs to Loki |

Confirm they are all Ready before testing:

```bash
kubectl --context kind-kind get pods
```

Startup order matters: Redis → validator → `account-init` → **miners →
relayers** (relayers depend on the miner-populated cache). Relayers stay
`Pending`/`Waiting` until miners are Ready — that is expected.

## 3. Port map (verified against the Tiltfiles)

Every port below is a Tilt port-forward from your workstation to the cluster.
**Do not assume round numbers** — these are the exact values from
`tilt/k8s/*.Tiltfile`. In particular, the relayer's relay and metrics host
ports are **not** the container ports.

| What | Host URL / addr | Container port | Source |
|---|---|---|---|
| Tilt UI | <http://localhost:10350> | — | `Tiltfile` |
| PATH gateway (relay entrypoint) | `http://localhost:3069/v1` | 3069 | `path.Tiltfile`, `defaults.Tiltfile` |
| PATH metrics | <http://localhost:9096> | 9096 | `path.Tiltfile` |
| **Relayer relay port** (HTTP/WS/gRPC/SSE) | `http://localhost:8180` | 8080 | `relayer.Tiltfile` (`base_port` 8180) |
| **Relayer metrics** | `http://localhost:9190/metrics` | 9090 | `relayer.Tiltfile` (`metrics_base_port` 9190) |
| **Relayer health** | `http://localhost:8280/health`, `/ready` | 8081 | `relayer.Tiltfile` (`health_base_port` 8280) |
| **Relayer pprof** | <http://localhost:6060/debug/pprof/> | 6060 | `relayer.Tiltfile` (`pprof_port` 6060) |
| **Miner metrics** | `http://localhost:9092/metrics` | 9092 | `miner.Tiltfile` (`metrics_base_port` 9092) |
| **Miner pprof** | <http://localhost:6065/debug/pprof/> | 6065 | `miner.Tiltfile` (`pprof_port` 6065) + `utils.Tiltfile` (bind `0.0.0.0:6065`) |
| **Prometheus** | <http://localhost:9091> | 9090 | `observability.Tiltfile` (`prometheus.port` 9091) |
| Grafana | <http://localhost:3000> | 3000 | `observability.Tiltfile` (anon admin / pw `admin`) |
| Loki | <http://localhost:3100> | 3100 | `observability.Tiltfile` |
| Validator RPC (CometBFT) | <http://localhost:26657> | 26657 | `validator.Tiltfile` |
| Validator gRPC | `localhost:9090` | 9090 | `validator.Tiltfile` |
| Validator REST | <http://localhost:1317> | 1317 | `validator.Tiltfile` |
| Redis | `localhost:6379` | 6379 | `redis.Tiltfile` |
| Backend HTTP / gRPC / metrics | `localhost:8545` / `50051` / `9095` | 8545 / 50051 / 9095 | `backend.Tiltfile` |
| Backend-2 HTTP / gRPC / metrics | `localhost:18545` / `60051` / `19095` | 8545 / 50051 / 9095 | `backend.Tiltfile` |
| nginx-backend | `localhost:8548` | 80 | `nginx-backend.Tiltfile` |

> **Prometheus is `:9091`, not `:9090`.** `scripts/README.md` currently lists
> the Prometheus endpoint as `localhost:9090/metrics` — that is wrong. `:9090`
> is the validator gRPC port; the Prometheus query UI/API is forwarded to
> **`:9091`** (container `9090`). Scrape the raw relayer/miner metrics at
> `:9190/metrics` and `:9092/metrics` respectively.

> **Single-pod forwards.** `relayer` and `miner` are each one Deployment with
> two replicas but a single port-forward set, so `localhost:8180` and
> `localhost:9092` reach **one** pod. To hit a specific replica use `kubectl`;
> for fleet-wide metrics use Prometheus (it scrapes every pod in-cluster on
> `:9090`/`:9092`).

## 4. Preflight smoke test

One line to confirm the gateway is serving and the whole relay path
(PATH → relayer → miner-populated cache → backend) is wired. The service is
selected with the `Target-Service-Id` header; `develop-http` is the localnet
JSON-RPC service.

```bash
# Expect: 200
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:3069/v1 \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
```

```bash
# Same request, but print the body so you can see the signed backend response
curl -s -X POST http://localhost:3069/v1 \
  -H "Content-Type: application/json" \
  -H "Target-Service-Id: develop-http" \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}'
```

A healthy body looks like
`{"id":1,"jsonrpc":"2.0","result":{"method":"eth_blockNumber","params":[],"status":"ok"}}`.
If you get `200` with an empty body, the relayer likely returned a `503` that
PATH masked — test the relayer directly (see [DIRECT_CLI.md](DIRECT_CLI.md)) to
see the real error.

## 5. HA / chaos / resilience suite

These scripts run **against a live Tilt localnet** and exercise failover,
leader election, and claim/proof integrity. All default to the `kind-kind`
context and the `localhost:3069` gateway. Run them from the project root.

```bash
# Chaos monkey: randomly kills relayer/miner pods, blips Redis, injects backend
# latency / network partitions / memory pressure. Run alongside a stress load.
./scripts/test-stress-max.sh &
./scripts/test-chaos.sh
#   env: CHAOS_INTERVAL (default 20s), DURATION (default 300s)
```

```bash
# Targeted leader-failover: kills the miner leader in the gap between claim
# flush and proof, forcing the follower to lazy-load the SMST tree from Redis
# to build the proof (regression guard for miner/smst_manager.go bug #4).
# Pass = lazy_loaded>0, ClaimCreated==ProofSubmitted, ClaimExpired==0.
DURATION=240 MAX_KILLS=3 ./scripts/test-chaos-leader-flush-gap.sh
```

```bash
# Quantitative failover: mid-flight miner scale-down (2 -> 1), then asserts
# on-chain claimed relays == loader-reported successes within MAX_DRIFT_PCT.
# Catches relay loss on failover and WAL double-counting.
DURATION=120 HTTP_RPS=300 ./scripts/test-quantitative-failover.sh
#   env: MAX_DRIFT_PCT (default 5), KILL_MODE=graceful|hard
```

```bash
# Rebalancer veto fix (issue #7): both miner replicas must end with a non-zero
# claimed_count and no "DRAIN ABORTED" / "release vetoed" in the logs.
# Run after both miners are Ready.
./scripts/verify-rebalance-fix.sh
```

```bash
# End-to-end claim payment: sends relays to develop-http for one supplier,
# watches the miner log for the claim TX hash, then queries block_results for
# EventClaimSettled to confirm the on-chain mint.
./scripts/verify-claim-payment.sh
```

Claim/proof timing (when a claim is expected on-chain, and why proofs may lag)
is documented in [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md); the
leaf/relay-count model is in [../CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md).

## 6. Where to look

```bash
# Logs — direct, but containers rotate under load and you lose history
kubectl --context kind-kind logs -l app=relayer -f
kubectl --context kind-kind logs -l app=miner   -f
```

```bash
# Logs — Loki (localhost:3100) is the durable option; survives pod restarts.
# Apps: relayer, miner, path, validator, backend. Example (last 10 min):
now_ns=$(date -d "now" +%s)000000000
start_ns=$(date -d "10 minutes ago" +%s)000000000
curl -sG 'http://localhost:3100/loki/api/v1/query_range' \
  --data-urlencode 'query={app="miner"} |= "claim"' \
  --data-urlencode "limit=20" \
  --data-urlencode "start=$start_ns" --data-urlencode "end=$now_ns" \
  | jq -r '.data.result[].values[][1]'
```

```bash
# Metrics — Prometheus query API/UI at :9091 (NOT :9090)
curl -sG 'http://localhost:9091/api/v1/query' \
  --data-urlencode 'query=up' | jq '.data.result'
# Raw scrape endpoints: relayer localhost:9190/metrics, miner localhost:9092/metrics
```

```bash
# Dashboards — Grafana at :3000 (anonymous Admin; admin password is "admin")
open http://localhost:3000     # or just browse to it
```

```bash
# Profiling — pprof: relayer :6060, miner :6065
go tool pprof http://localhost:6060/debug/pprof/heap
go tool pprof "http://localhost:6065/debug/pprof/profile?seconds=30"
```

For Redis inspection (sessions, SMST trees, leader lock, submission tracking),
use the built-in `redis` subcommand — full reference in
[../REDIS.md](../REDIS.md):

```bash
pocket-relay-miner redis leader
pocket-relay-miner redis keys --pattern "ha:*" --stats
pocket-relay-miner redis submissions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
```

## 7. Sending relays

Once the smoke test passes, drive real traffic with the CLI:
`pocket-relay-miner relay <mode> --localnet` talks straight to a relayer replica
on `:8180`, verifies the supplier signature and the backend's own error field,
and reports honest per-relay results — for single relays and for sustained load
alike. See [DIRECT_CLI.md](DIRECT_CLI.md).

The PATH gateway runs in the localnet so the production routing path exists, but
do **not** measure relays through it: PATH answers a relayer `503` with `200`
and an empty body, so a gateway-side load tool reports success for relays that
were never mined.

## See also

- [README.md](README.md) — testing docs index.
- [DIRECT_CLI.md](DIRECT_CLI.md) — direct relay testing via the CLI.
- [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) — claim/proof windows.
- [../CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md) — the SMST leaf / relay-count model.
- [../REDIS.md](../REDIS.md) — the `redis` debug subcommands in depth.
