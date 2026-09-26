# Deploy with Docker Compose

This runbook starts the compose example in
[examples/docker-compose/](../../examples/docker-compose/): Redis 8.10.1,
1 relayer and 1 miner, pointed at the **beta testnet** (chain id
`pocket-lego-testnet`) through the public Sauron endpoints. No chain runs
locally; for a local chain to develop on, use Tilt
([docs/testing/TILT.md](../testing/TILT.md)).

The runbook goes in two halves:

1. **Steps 0 to 8**: first start, with the PUBLIC, unstaked key that ships in
   `config/supplier-keys.yaml`. Nothing is at stake. It proves Redis, the node
   connection and both processes.
2. **Steps 9 to 14**: your own keys, your services and backends, then real
   relays and their claims and proofs. **Stop and ask a human** before step 9.

Mainnet is the same runbook with the values in
[Switching to mainnet](#switching-to-mainnet).

Every **Expect** below comes from a real run against beta on 2026-09-26, unless
it says **not verified**. That run used an image built locally from commit
`daaf6e2`, because `ghcr.io/pokt-network/pocket-relay-miner:v0.1.0` was not
published yet, and a random key that is not staked anywhere, so its supplier
address differs from the one the shipped key gives. Ids, timestamps, heights
and durations differ on every run, and `...` marks lines left out.

How to read a step: **Run** the command, compare with **Expect**, use
**If not** when it differs, and **Stop if** says when to ask a human instead of
continuing. Details for every error message are in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Step 0: prerequisites

Run every command from the repository root. Set this once per shell:

```bash
export C="docker compose -p prm-example -f examples/docker-compose/docker-compose.yaml"
```

**Run**

```bash
docker compose version
```

**Expect**: `Docker Compose version v2.` or later (`v5.5.1` was used).

**If not**: `docker: 'compose' is not a docker command` → Compose v2 plugin
missing → install Docker Engine with the compose plugin.

**Stop if**: you cannot install Docker on this machine.

Also needed: about 10 GiB of free RAM (Redis and the miner are limited to
4 GiB each, the relayer to 2 GiB), the
local port 8180 free (or export `RELAYER_PORT` before step 4), and outbound
HTTPS to `sauron-rpc.beta.infra.pocket.network` and
`sauron-grpc.beta.infra.pocket.network:443`.

## Step 1: get the image

**Run**

```bash
docker pull ghcr.io/pokt-network/pocket-relay-miner:v0.1.0
```

**Expect**: exit 0 and `Status: Downloaded newer image` or
`Status: Image is up to date`. **Not verified**: the tag was not published
when this runbook was checked.

**If not**: `manifest unknown` → the tag is not published yet → build it
locally with the same name (takes a few minutes), because the compose file
names that image:

```bash
git checkout v0.1.0 2>/dev/null || echo "tag v0.1.0 not found: building the current checkout"
docker build -t ghcr.io/pokt-network/pocket-relay-miner:v0.1.0 .
```

If the tag is not in the repository either, the release is not out: the image
is then whatever commit you have checked out, under the v0.1.0 name.

**Stop if**: someone asks you to use another tag. Relayer and miner must run
the same version.

## Step 2: the node answers, on the network you expect

**Run**

```bash
curl -s https://sauron-rpc.beta.infra.pocket.network/status | grep -oE '"network":"[^"]*"|"latest_block_height":"[0-9]*"'
```

**Expect**

```
"network":"pocket-lego-testnet"
"latest_block_height":"680249"
```

Run it again about 30 seconds later: the height grows by about 1 (beta makes a
block about every 30 seconds).

**If not**: no output → the node is unreachable from this machine → check
outbound HTTPS. Another `network` → it is not the node you meant; the miner
refuses to start on a chain id that differs from `pocket_node.chain_id`.

## Step 3: validate both configs

**Run**

```bash
$C run --rm --no-deps miner miner validate --config /config/miner.yaml; echo "EXIT=$?"
$C run --rm --no-deps relayer relayer validate --config /config/relayer.yaml; echo "EXIT=$?"
```

**Expect** (container ids and timestamps differ)

```
 Container prm-example-miner-run-... Created
2026/09/26 07:46:06 maxprocs: Honoring GOMAXPROCS="2" as set in environment
config OK: /config/miner.yaml would start
EXIT=0
 Container prm-example-relayer-run-... Created
2026/09/26 07:46:07 maxprocs: Honoring GOMAXPROCS="2" as set in environment
config OK: /config/relayer.yaml would start
EXIT=0
```

The lines that matter are `config OK: ... would start` and `EXIT=0`.

**If not**: `Error: config is INVALID: ...` → the message names the key (and
the line, for an unknown or retired key) → fix that key in
`examples/docker-compose/config/`. See
[Config rejected](TROUBLESHOOTING.md#config-rejected).

**Stop if**: the fix would mean removing a key you do not understand.

## Step 4: first start, with the public unstaked key

`config/supplier-keys.yaml` ships 1 PUBLIC key, supplier 2 of the Tilt
localnet (`pokt1re27pw4llwnatx4sq7rlggqzcm6j3f39epq2wa`). It was not staked on
beta or mainnet and held no funds when checked on 2026-09-26. The stack starts
with it and serves nothing, which is the point: every part except the stake
is exercised. Never fund or stake that key.

**Run**

```bash
$C up -d; echo "EXIT=$?"
```

**Expect**: the last lines are

```
 Container prm-example-miner-1 Healthy
 Container prm-example-relayer-1 Starting
 Container prm-example-relayer-1 Started
EXIT=0
```

**If not**:
- `container prm-example-miner-1 is unhealthy` →
  `$C logs miner | grep '"level":"error"\|Error:'` and look the message up in
  [Miner does not start](TROUBLESHOOTING.md#miner-does-not-start).
- `bind: address already in use` → port 8180 is taken → export
  `RELAYER_PORT=18180`, run `$C down -v`, retry.

**Stop if**: the same step fails twice after a [reset](#reset).

## Step 5: every service is healthy

**Run**

```bash
$C ps -a --format '{{.Service}}\t{{.Status}}'
```

**Expect** (about a minute after step 4)

```
miner	Up About a minute (healthy)
redis	Up About a minute (healthy)
relayer	Up About a minute (healthy)
```

**If not**: `miner ... Restarting` → the miner cannot read the chain →
[Miner does not start](TROUBLESHOOTING.md#miner-does-not-start).
`relayer ... (unhealthy)` → its `/ready` stays 503 → step 8.

## Step 6: Redis runs with the required memory settings

**Run**

```bash
$C exec redis redis-cli --raw CONFIG GET maxmemory-policy
$C exec redis redis-cli --raw CONFIG GET maxmemory
```

**Expect**

```
maxmemory-policy
noeviction
maxmemory
3221225472
```

**If not**: any other policy, or `0` → the Redis config was not loaded → both
binaries refuse to start; see [Redis](TROUBLESHOOTING.md#redis).

## Step 7: the miner reads the chain and sees the supplier as unstaked

**Run**

```bash
$C logs --no-log-prefix miner | grep -E 'using chain ID|fetched initial block|WebSocket subscription established|supplier manager started'
$C exec miner pocket-relay-miner redis --config /config/miner.yaml supplier --list | grep -v INF
```

**Expect** (log lines shortened)

```
{"level":"info",...,"chain_id":"pocket-lego-testnet",...,"message":"using chain ID for transaction signing"}
{"level":"info",...,"claimed":0,"staked_suppliers":0,"total_keys":1,...,"message":"supplier manager started with distributed claiming"}
{"level":"info",...,"height":680246,"block_time":"2026-09-26T07:45:53Z",...,"message":"fetched initial block via RPC"}
{"level":"info",...,"message":"WebSocket subscription established"}
...
Supplier Cache (1 total: 0 staked, 1 not staked):

ADDRESS                                      STATUS      STAKED  SERVICES  LAST UPDATED
───────                                      ──────      ──────  ────────  ────────────
pokt108cdyngrx0x8sh8pgagwk6d574hly9j5764pyp  not_staked  ✗ no    -         2026-09-26 07:47:31
```

With the shipped key the address is `pokt1re27pw4llwnatx4sq7rlggqzcm6j3f39epq2wa`.
`staked_suppliers:0` and `not_staked` are expected here: an unstaked key is
not an error, and neither process logged a warning or an error in the recorded
run.

**If not**: no `fetched initial block` line → the miner cannot reach the RPC
URL → check `pocket_node.query_node_rpc_url` and step 2.

## Step 8: the relayer is up and receives blocks

**Run**

```bash
$C exec relayer curl -s -w ' HTTP=%{http_code}\n' http://localhost:8081/health
$C exec relayer curl -s -w ' HTTP=%{http_code}\n' http://localhost:8081/ready
$C exec relayer curl -s http://localhost:9090/metrics | grep -E '^ha_relayer_current_block_height'
```

**Expect**

```
OK HTTP=200
READY HTTP=200
ha_relayer_current_block_height 680249
```

Run the last command again a minute later: the height grows. The relayer gets
its blocks from the miner through Redis, so a growing height proves the whole
chain → miner → Redis → relayer path. `READY` means the relayer can serve; it
does not mean any supplier is staked: with the public key it serves nothing.

**If not**: `no service factor manifest: the miner has not published one yet HTTP=503`
→ the miner is not running or has not published yet → step 5; see
[Relayer up but not ready](TROUBLESHOOTING.md#relayer-up-but-not-ready).

This is the end of the first half. Relays served, claims and proofs need
your own staked supplier.

## Step 9: switch to your own keys

**Stop if**: you do not have a staked supplier's private key from a human.
An agent never generates, funds or stakes a key, and never pastes a key into a
tracked file.

`config/supplier-keys.yaml` is tracked and holds only the public key. Your keys
go in `config/supplier-keys.local.yaml`, which the example's `.gitignore`
excludes.

**Run** (from `examples/docker-compose/`)

```bash
cp config/supplier-keys.yaml config/supplier-keys.local.yaml
$EDITOR config/supplier-keys.local.yaml     # replace the key under keys: with yours, 1 per supplier
chmod 0600 config/supplier-keys.local.yaml
sudo chown 1000:1000 config/supplier-keys.local.yaml
git check-ignore config/supplier-keys.local.yaml
```

The image runs as uid 1000, so a 0600 file owned by any other uid cannot be
read. Then, in `docker-compose.yaml`, change both keys mounts (under `miner`
and under `relayer`) from `./config/supplier-keys.yaml` to
`./config/supplier-keys.local.yaml`. For a keyring instead of a keys file, see
[docs/SUPPLIER_KEYS.md](../SUPPLIER_KEYS.md).

**Expect**: `git check-ignore` prints `config/supplier-keys.local.yaml`: git
will not track it. **Not verified** with a real key; the ignore rule itself
was checked.

**If not**: `git check-ignore` prints nothing → the file would be committable
→ do not continue until it is ignored.

## Step 10: your services and backends

In `config/relayer.yaml`, replace `my-service` with the on-chain service id
your supplier is staked for, and its `backends.jsonrpc.url` with your
backend's URL, reachable from inside the relayer container. Add 1 entry under
`services:` per staked service; [config.relayer.example.yaml](../../config.relayer.example.yaml)
documents every option, including the other transports.

## Step 11: validate, and check the stake against your backends

**Run**

```bash
$C run --rm --no-deps relayer relayer validate --config /config/relayer.yaml --check-stake; echo "EXIT=$?"
$C run --rm --no-deps miner miner validate --config /config/miner.yaml; echo "EXIT=$?"
```

**Expect** with your staked keys: `config OK`, then
`stake check OK: every staked (service, transport) pair has a backend` and
`EXIT=0`. **Not verified** with a staked key. The recorded run, with the
unstaked key, printed:

```
config OK: /config/relayer.yaml would start
...
checking on-chain stake for 1 supplier(s) via sauron-grpc.beta.infra.pocket.network:443 (tls=true)
  info   supplier not staked on-chain (nothing to serve): supplier=pokt108cdyngrx0x8sh8pgagwk6d574hly9j5764pyp
  info   backend configured, not staked (surplus): service=my-service transport=jsonrpc
stake check OK: every staked (service, transport) pair has a backend
...
EXIT=0
```

**If not**:
- `ERROR  staked but no backend: supplier=... service=... transport=...` →
  a pair you are staked for and do not serve, which earns nothing → add it in
  step 10, or stop and ask.
- `info   supplier not staked on-chain` for YOUR key → the key is not the
  staked one, or it is staked on the other network.

**Stop if**: you are not sure which network the supplier is staked on.

## Step 12: restart with your keys and services

**Run**

```bash
$C up -d --force-recreate miner relayer; echo "EXIT=$?"
```

Then repeat steps 5, 7 and 8.

**Expect** (the command itself, from the recorded run)

```
 Container prm-example-miner-1 Recreated
 Container prm-example-relayer-1 Recreated
...
 Container prm-example-miner-1 Healthy
 Container prm-example-relayer-1 Started
EXIT=0
```

Then steps 5 and 8 as before, and in step 7 `"staked_suppliers":1` (or your
count) and the supplier list showing your addresses as `staked`. **Not
verified** with a staked key.

## Step 13: send relays

Either:
- a **simulated relay** from your own tooling: real signature, real backend
  round-trip, never metered or claimed. It needs a simulation identity in
  `config/relayer.yaml`; see [docs/SIMULATED_RELAYS.md](../SIMULATED_RELAYS.md).
- a **real gateway** sending relays to the relayer, once your supplier's staked
  endpoint URL points at it. Publish the relay port first: the `relayer`
  service binds `127.0.0.1` only; bind the address your gateways reach, behind
  your TLS proxy. Never publish Redis.

**Not verified** on beta: the recorded run had no staked supplier and no real
backend. What a successful relay looks like from the command-line client is in
[docs/testing/DIRECT_CLI.md](../testing/DIRECT_CLI.md).

## Step 14: watch claims and proofs

A claim is submitted after the session of the relays ends, and the proof after
the claim, each in its on-chain window.

**Run** (every few minutes)

```bash
$C exec miner pocket-relay-miner redis --config /config/miner.yaml submissions --supplier <your-supplier-address> | grep -v INF
```

**Expect**: before any claim, `No submission tracking records found` (the
recorded run). Once your relays are claimed, 1 row per session with
`CLAIM_STATUS` and then `PROOF_STATUS` at `✓ SUCCESS`, as in:

```
SESSION_END  SERVICE       CLAIM_STATUS  PROOF_STATUS  RELAYS  CU      SESSION_ID
-----------  -------       ------------  ------------  ------  --      ----------
20           develop-http  ✓ SUCCESS     ✓ SUCCESS     301     301000  fce30d03f194...
```

That row is from a local chain, not from beta. **Not verified** on beta:
claims, proofs and their transaction broadcast through Sauron.

**Stop if**: a claim or proof shows `FAILED`; report the row and
`$C logs miner | grep '"level":"error"'`.

## Reset

**Run**

```bash
$C down -v; echo "EXIT=$?"
```

**Expect**: `Volume prm-example_redis-data Removed` and `EXIT=0`.

`-v` deletes the Redis data: the relays not yet claimed and the claim trees of
sessions not yet proved. Do not reset while a claimed session awaits its proof.

## Switching to mainnet

**Stop if**: you have not been told by a human to run on mainnet.

Every network-specific value has its mainnet value in a comment right above
it, marked `Mainnet:`:

- `config/relayer.yaml` and `config/miner.yaml`:
  `pocket_node.query_node_rpc_url` (`https://sauron-rpc.infra.pocket.network`)
  and `pocket_node.query_node_grpc_url` (`sauron-grpc.infra.pocket.network:443`).
- `config/miner.yaml`: `pocket_node.chain_id` (`pocket`) and
  `block_time_seconds` (`60`; mainnet makes a block about every 61 seconds).

Then run the whole runbook again from step 2, with
`https://sauron-rpc.infra.pocket.network/status` in step 2 (**Expect**
`"network":"pocket"`, measured on 2026-09-26). A read-only
`--check-stake` against the mainnet gRPC with an unstaked key printed the same
lines as in step 11; starting the stack on mainnet was **not verified**.

The public Sauron endpoints are enough to start. For claims and proofs you are
paid for, your own full node is the better choice: node configs and genesis
per network (`mainnet`, and `testnet-lego` for beta) are in
[pocket-network-genesis/shannon](https://github.com/pokt-network/pocket-network-genesis/tree/master/shannon),
and snapshots and other public endpoints in
[pocket-network-resources](https://github.com/pokt-network/pocket-network-resources).

The limits in `docker-compose.yaml` (Redis 4 GiB with a 3 GiB `maxmemory`,
miner 4 GiB, relayer 2 GiB, 2 CPUs each) fit a deployment with a few
suppliers. Above each one, a comment gives what the v0.1.0 load run
(50 suppliers, ~2,400 relays/s;
[capacity report](../benchmarks/v0.1.0/Relay-Miner-Capacity.pdf), read with
[docs/benchmarks/README.md](../benchmarks/README.md)) used and what to set at
that scale. When you
change them, keep `mem_limit` of Redis at least 1.25x its `maxmemory`, and
`GOMEMLIMIT` about 10% below each process's `mem_limit`.
