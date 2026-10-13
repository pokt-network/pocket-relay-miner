# Deploy with Docker Compose: standalone mode

This runbook starts the **standalone mode** of the compose example in
[examples/docker-compose/](../../examples/docker-compose/)
(`docker-compose.standalone.yaml`): 1 process that runs the relayer and the
miner, their state in an embedded store on a Docker volume, and **no Redis**,
pointed at the **beta testnet** (chain id `pocket-lego-testnet`) through the
public Sauron endpoints.

For the **high-availability mode** (Redis, relayers and miners as separate
processes, replicas and failover), follow [DOCKER_COMPOSE.md](DOCKER_COMPOSE.md)
instead. [README.md, "Choose a mode"](README.md#choose-a-mode) compares the
two.

The steps have the same numbers as in the high-availability runbook, and go in
the same two halves:

1. **Steps 0 to 8**: first start, with the PUBLIC, unstaked key that ships in
   `config/supplier-keys.yaml`. Nothing is at stake. It proves the store, the
   node connection and both sides of the process.
2. **Steps 9 to 14**: your own keys, your services and backends, then real
   relays and their claims and proofs. **Stop and ask a human** before step 9.

**What is verified.** Standalone mode is newer than the high-availability
runbook's recorded beta run, and its own beta run has not been recorded. Each
**Expect** below says where it comes from: **checked** (run on 2026-10-07
against this repository, without a chain), **as in high availability** (the
same code prints the same line; the high-availability runbook recorded it on
beta), or **not verified**.

How to read a step: **Run** the command, compare with **Expect**, use
**If not** when it differs, and **Stop if** says when to ask a human instead of
continuing. Details for every error message are in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Step 0: prerequisites

Run every command from the repository root. Set this once per shell:

```bash
export C="docker compose -p prm-standalone -f examples/docker-compose/docker-compose.standalone.yaml"
```

**Run**

```bash
docker compose version
```

**Expect**: `Docker Compose version v2.` or later.

**If not**: `docker: 'compose' is not a docker command` → Compose v2 plugin
missing → install Docker Engine with the compose plugin.

**Stop if**: you cannot install Docker on this machine.

This stack needs ports 8180 (relays), 9091 (Prometheus) and 3000 (Grafana)
free on this host.

**Run**

```bash
ss -ltn | grep -E ':(8180|9091|3000) ' ; echo "EXIT=$?"
```

**Expect**: no port lines and `EXIT=1`.

**Stop if**: any line is printed: another program holds that port. Tell the
human which port; they free it, or set other ports in
`examples/docker-compose/.env` (`RELAYER_PORT`, `PROMETHEUS_PORT`,
`GRAFANA_PORT`), and use those wherever this runbook says 8180, 9091 or 3000.

The store lives on the disk that holds Docker's volumes, and the process stops
taking new work when that disk has less than 1 GiB free.

**Run**

```bash
df -h "$(docker info -f '{{.DockerRootDir}}')"
```

**Expect**: an `Avail` of several GiB. **Stop if**: less than 2 GiB is free.

Also needed: about 6 GiB of free RAM (the container is limited to 6 GiB;
`free -g` shows it in the `available` column), and outbound HTTPS to
`sauron-rpc.beta.infra.pocket.network` and
`sauron-grpc.beta.infra.pocket.network:443`.

## Step 1: build the image

Standalone mode is not in `v0.1.2`, the release the high-availability example
pins. The compose file builds the image from this checkout, under the name
`pocket-relay-miner:standalone-local`.

**Run**

```bash
$C build; echo "EXIT=$?"
```

**Expect**: the build steps, then `EXIT=0`. **Not verified**: the build on
2026-10-07 was refused by Docker Hub's anonymous pull limit
(`429 Too Many Requests` on `alpine:latest`).

**If not**: `429 Too Many Requests` → Docker Hub's pull limit → wait, or
`docker login`, and run it again.

**Stop if**: someone asks you to use an image of another version than this
checkout.

## Step 2: the node answers, on the network you expect

**Run**

```bash
curl -s https://sauron-rpc.beta.infra.pocket.network/status | grep -oE '"network":"[^"]*"|"latest_block_height":"[0-9]*"'
```

**Expect** (as in high availability)

```
"network":"pocket-lego-testnet"
"latest_block_height":"680249"
```

Run it again about 30 seconds later: the height grows by about 1.

**If not**: no output → the node is unreachable from this machine → check
outbound HTTPS. Another `network` → it is not the node you meant; the process
refuses to start on a chain id that differs from `pocket_node.chain_id`.

## Step 3: validate the config

**Run**

```bash
$C run --rm --no-deps standalone standalone validate --config /config/standalone.yaml; echo "EXIT=$?"
```

**Expect** (checked, with the binary of this checkout; the container prints a
`Container ... Created` line first)

```
config OK: /config/standalone.yaml would start
EXIT=0
```

**If not**: `Error: config is INVALID: ...` → the message names the key (and
the line, for an unknown key) → fix that key in
`examples/docker-compose/config/standalone.yaml`. `a standalone config has no
redis section` → a `redis:` section was added at the top level: remove it;
standalone mode uses no Redis.

**Stop if**: the fix would mean removing a key you do not understand.

## Step 4: first start, with the public unstaked key

`config/supplier-keys.yaml` ships 1 PUBLIC key
(`pokt1re27pw4llwnatx4sq7rlggqzcm6j3f39epq2wa`), not staked on beta or mainnet.
The process starts with it and serves nothing. Never fund or stake that key.

**Run**

```bash
$C up -d; echo "EXIT=$?"
```

**Expect** (not verified): the last lines are
`Container prm-standalone-standalone-1 Started` and `EXIT=0`.

**If not**: `bind: address already in use` → a port is taken → step 0.

**Stop if**: the same step fails twice after a [reset](#reset).

## Step 5: the process is healthy

**Run**

```bash
$C ps -a --format '{{.Service}}\t{{.Status}}'
```

**Expect** (not verified; about a minute after step 4)

```
standalone	Up About a minute (healthy)
```

**If not**: `Restarting` → the process cannot read the chain →
`$C logs standalone | grep '"level":"error"\|Error:'` and look the message up
in [Miner does not start](TROUBLESHOOTING.md#miner-does-not-start).
`(unhealthy)` → `/ready` stays 503 → step 8.

## Step 6: the store has room

Standalone mode has no Redis to check. Its store is the volume, and the process
reports the free space it sees there.

**Run**

```bash
$C exec standalone df -h /home/pocket/.pocket-relay-miner/data
$C exec standalone curl -s http://localhost:9092/metrics | grep -E '^ha_transport_store_(free_bytes|operable)'
```

**Expect** (not verified): `df` shows the volume with several GiB available,
`ha_transport_store_free_bytes` for `component="miner"` and
`component="relayer"` is that free space in bytes, and both
`ha_transport_store_operable` series are `1`.

**If not**: `operable` is `0` → the disk is nearly full → free space on it; the
process takes work again with 2 GiB free.

## Step 7: the miner side reads the chain and sees the supplier as unstaked

**Run**

```bash
$C logs --no-log-prefix standalone | grep -E 'using chain ID|fetched initial block|WebSocket subscription established|supplier manager started'
```

**Expect** (as in high availability; log lines shortened)

```
{"level":"info",...,"chain_id":"pocket-lego-testnet",...,"message":"using chain ID for transaction signing"}
{"level":"info",...,"claimed":0,"staked_suppliers":0,"total_keys":1,...,"message":"supplier manager started with distributed claiming"}
{"level":"info",...,"height":680246,...,"message":"fetched initial block via RPC"}
{"level":"info",...,"message":"WebSocket subscription established"}
```

`staked_suppliers:0` is expected here: an unstaked key is not an error. The
`pocket-relay-miner redis ...` inspection commands of the high-availability
runbook read Redis; standalone mode has none, so this runbook reads the logs
and the metrics instead.

**If not**: no `fetched initial block` line → the process cannot reach the RPC
URL → check `pocket_node.query_node_rpc_url` and step 2.

## Step 8: the relayer side is up and receives blocks

**Run**

```bash
$C exec standalone curl -s -w ' HTTP=%{http_code}\n' http://localhost:8081/health
$C exec standalone curl -s -w ' HTTP=%{http_code}\n' http://localhost:8081/ready
$C exec standalone curl -s http://localhost:9092/metrics | grep -E '^ha_relayer_current_block_height'
```

**Expect** (as in high availability; both sides' metrics are on port 9092)

```
OK HTTP=200
READY HTTP=200
ha_relayer_current_block_height 680249
```

The relayer side logs 1 warning, `backend became unhealthy (active health
check)`, for the example's placeholder backend: expected until step 10 sets
your backend.

Run the last command again a minute later: the height grows. The relayer side
gets its blocks from the miner side inside the process, so a growing height
proves the chain → miner → relayer path.

**If not**: `no service factor manifest: the miner has not published one yet
HTTP=503` → the miner side has not published yet → step 5.

This is the end of the first half. Relays served, claims and proofs need your
own staked supplier.

## Dashboards (optional, any time from here)

```bash
$C --profile observability up -d
```

Prometheus scrapes the one process once, as job `standalone`; see
[examples/observability/README.md](../../examples/observability/README.md).
The Redis panels stay empty: there is no Redis; dashboard 8 (Standalone
store) shows the embedded store and the relay queue instead. **Not verified**:
the dashboards on a standalone process. To stop everything,
`$C --profile observability down -v`.

## Step 9: switch to your own keys

**Stop if**: you do not have a staked supplier's private key from a human.
An agent never generates, funds or stakes a key, never asks for one in the
chat, and never writes one into a file: the human does, with their own key.
[docs/SUPPLIER_KEYS.md, "Creating a supplier key, and staking it"](../SUPPLIER_KEYS.md#creating-a-supplier-key-and-staking-it).

**Run** (from `examples/docker-compose/`)

```bash
cp config/supplier-keys.yaml config/supplier-keys.local.yaml
$EDITOR config/supplier-keys.local.yaml     # replace the key under keys: with yours, 1 per supplier
chmod 0600 config/supplier-keys.local.yaml
sudo chown 1000:1000 config/supplier-keys.local.yaml
git check-ignore config/supplier-keys.local.yaml
```

Then, in `docker-compose.standalone.yaml`, change the keys mount from
`./config/supplier-keys.yaml` to `./config/supplier-keys.local.yaml` (there is
one: the process loads the keys once for both sides).

**Expect**: `git check-ignore` prints `config/supplier-keys.local.yaml`.

**If not**: it prints nothing → the file would be committable → do not continue
until it is ignored.

## Step 10: your services and backends

Find the service each of your nodes serves, and check it exists on this
network, exactly as in
[DOCKER_COMPOSE.md, step 10](DOCKER_COMPOSE.md#step-10-your-services-and-backends).
The one difference: the services are in `config/standalone.yaml`, under
`relayer:` → `services:`.

**Run**, for each backend URL (a JSON-RPC probe; use your backend's own health
request otherwise):

```bash
$C exec standalone curl -s -m 5 -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' <backend-url>; echo " EXIT=$?"
```

**Expect**: a JSON body with `"result"` and `EXIT=0`.

## Step 11: validate, and check the stake against your backends

**Run**

```bash
$C run --rm --no-deps standalone standalone validate --config /config/standalone.yaml --check-stake; echo "EXIT=$?"
```

**Expect** with your staked keys: `config OK`, then
`stake check OK: every staked (service, transport) pair has a backend` and
`EXIT=0`, the same check as `relayer validate --check-stake`. **Not verified**
with a staked key.

**If not**: `ERROR  staked but no backend: ...` → a pair you are staked for and
do not serve → add it in step 10, or stop and ask.

**Stop if**: you are not sure which network the supplier is staked on.

## Step 12: restart with your keys and services

**Run**

```bash
$C up -d --force-recreate standalone; echo "EXIT=$?"
```

Then repeat steps 5, 7 and 8; in step 7, `"staked_suppliers":1` (or your
count). **Not verified** with a staked key.

## Step 13: send relays

As in [DOCKER_COMPOSE.md, step 13](DOCKER_COMPOSE.md#step-13-send-relays): a
simulated relay, or a real gateway once your staked endpoint URL points at the
relay port. **Not verified** in standalone mode on beta.

## Step 14: watch claims and proofs

A claim is submitted after the session of the relays ends, and the proof after
the claim, each in its on-chain window.

**Run** (every few minutes)

```bash
$C exec standalone curl -s http://localhost:9092/metrics | grep -E '^ha_miner_(claims_submitted|claim_errors|proofs_submitted|proof_errors)_total'
```

**Expect**: no lines before the first claim; then `ha_miner_claims_submitted_total`
and `ha_miner_proofs_submitted_total` grow, per supplier and service, and the
`_errors_total` series stay absent or flat. **Not verified** on beta.

**Stop if**: an `_errors_total` series grows; report it with
`$C logs standalone | grep '"level":"error"'`.

## Reset

**Run**

```bash
$C down -v; echo "EXIT=$?"
```

**Expect**: `Volume prm-standalone_standalone-data Removed` and `EXIT=0`.

`-v` deletes the store: the relays not yet claimed and the claim trees of
sessions not yet proved. Do not reset while a claimed session awaits its proof.

## Switching to mainnet

**Stop if**: you have not been told by a human to run on mainnet.

Every network-specific value in `config/standalone.yaml` has its mainnet value
in a comment right above it, marked `Mainnet:`: `pocket_node.query_node_rpc_url`,
`pocket_node.query_node_grpc_url`, `pocket_node.chain_id` (`pocket`) and
`miner.block_time_seconds` (`60`). Then run the whole runbook again from step 2,
with `https://sauron-rpc.infra.pocket.network/status` in step 2. **Not
verified** on mainnet.

The limit in `docker-compose.standalone.yaml` (6 GiB, 2 CPUs) is the sum of the
high-availability example's relayer and miner, not a measurement of standalone
mode. Watch the container's memory as traffic grows.
