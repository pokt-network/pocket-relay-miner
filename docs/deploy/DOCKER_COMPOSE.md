# Deploy with Docker Compose

This runbook starts the compose example in
[examples/docker-compose/](../../examples/docker-compose/): a local 1-validator
chain, Redis 8.10, a test backend, 1 relayer and 1 miner. It ends with a relay
served, its claim and proof on chain, and the supplier's reward settled. Then
[Pointing it at your own node](#pointing-it-at-your-own-node) says what to
change for a real network.

Every **Expect** below is copied from a real run of this runbook on
2026-09-26 (image built from commit `334a4c4`).

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

Also needed: about 6 GB of free RAM (3 containers are limited to 2 GB each,
plus the validator and the backend) and free local ports 8180 and 9090. To use
other ports, export `RELAYER_PORT` and `VALIDATOR_GRPC_PORT` before step 3.

## Step 1: get the image

**Run**

```bash
docker pull ghcr.io/pokt-network/pocket-relay-miner:v0.1.0
```

**Expect**: exit 0 and `Status: Downloaded newer image` or
`Status: Image is up to date`.

**If not**: `manifest unknown` → the tag is not published yet → build it
locally with the same name (takes a few minutes):

```bash
docker build -t ghcr.io/pokt-network/pocket-relay-miner:v0.1.0 .
```

**Stop if**: someone asks you to use another tag. Relayer and miner must run
the same version, and v0.1.0 is what this runbook was verified with.

## Step 2: validate both configs

**Run**

```bash
$C run --rm --no-deps miner miner validate --config /config/miner.yaml; echo "EXIT=$?"
$C run --rm --no-deps relayer relayer validate --config /config/relayer.yaml; echo "EXIT=$?"
```

**Expect**

```
config OK: /config/miner.yaml would start
EXIT=0
config OK: /config/relayer.yaml would start
EXIT=0
```

**If not**: `Error: config is INVALID: ...` → the message names the key and
the line → fix that key in `examples/docker-compose/config/`. See
[Config rejected](TROUBLESHOOTING.md#config-rejected).

**Stop if**: the fix would mean removing a key you do not understand.

## Step 3: start everything

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

The first run builds the test backend image, which takes a few minutes.

**If not**:
- `account-init` exited 1 → the chain did not register the account keys
  → `$C logs account-init | tail -5` (the last line names the accounts
  without a public key), then [reset](#reset) and retry once.
- `container prm-example-miner-1 is unhealthy` → `$C logs miner | grep '"level":"error"\|Error:'`
  and look the message up in [TROUBLESHOOTING.md](TROUBLESHOOTING.md#miner-does-not-start).
- `bind: address already in use` → port 8180 or 9090 is taken → export
  `RELAYER_PORT=18180` or `VALIDATOR_GRPC_PORT=19090`, run `$C down -v`, retry.

**Stop if**: the same step fails twice after a reset.

## Step 4: every service is healthy

**Run**

```bash
$C ps -a --format '{{.Service}}\t{{.Status}}'
```

**Expect** (seconds vary; right after step 3 the relayer shows
`(health: starting)` and turns `(healthy)` within about 30 seconds):

```
account-init	Exited (0) 7 seconds ago
backend	Up 51 seconds (healthy)
miner	Up 6 seconds (healthy)
redis	Up 52 seconds (healthy)
relayer	Up 30 seconds (healthy)
validator	Up 51 seconds (healthy)
```

**If not**: `relayer ... (unhealthy)` → its `/ready` stays 503 → step 7.
`miner ... Restarting` → the miner cannot read the chain →
[Miner does not start](TROUBLESHOOTING.md#miner-does-not-start).

## Step 5: the chain produces blocks

**Run** (twice, about 30 seconds apart)

```bash
$C exec validator pocketd status --node tcp://localhost:26657 | grep -o '"latest_block_height":"[0-9]*"'
```

**Expect**: the height grows by about 3 every 30 seconds (the local chain
makes a block every ~11 seconds), for example `"latest_block_height":"5"` then
`"latest_block_height":"8"`.

**If not**: the height does not move → `$C logs validator | tail -20`.

**Stop if**: the validator logs a genesis or consensus error.

## Step 6: Redis runs with the required memory settings

**Run**

```bash
$C exec redis redis-cli CONFIG GET maxmemory-policy
$C exec redis redis-cli CONFIG GET maxmemory
```

**Expect**

```
maxmemory-policy
noeviction
maxmemory
1073741824
```

**If not**: any other policy, or `0` → the Redis config was not loaded → both
binaries refuse to start; see [Redis](TROUBLESHOOTING.md#redis).

## Step 7: the relayer is ready

**Run**

```bash
$C exec relayer curl -s -w ' HTTP=%{http_code}\n' http://localhost:8081/ready
```

**Expect**: `READY HTTP=200`

**If not**: `no service factor manifest: the miner has not published one yet HTTP=503`
→ the miner is not running or has not published yet → check step 4 for the
miner; see [Relayer up but not ready](TROUBLESHOOTING.md#relayer-up-but-not-ready).

## Step 8: build the relay test client

The relay client is the same binary, run on the host. `--localnet` reads the
local chain's test application and gateway keys from `tilt/config/`, so run it
from the repository root.

**Run** (needs Go 1.26.5 and make)

```bash
make build
```

**Expect**

```
Building pocket-relay-miner...
Build complete: ./bin/pocket-relay-miner
```

**If not**: no Go toolchain → copy the binary out of the image instead (it is
statically linked):

```bash
mkdir -p bin && id=$(docker create ghcr.io/pokt-network/pocket-relay-miner:v0.1.0) && docker cp "$id:/usr/local/bin/pocket-relay-miner" bin/pocket-relay-miner && docker rm "$id"
```

## Step 9: record the supplier's balance

A balance only means something against a baseline, so take it before the relay.
`pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj` is localnet supplier 1.

**Run**

```bash
$C exec validator pocketd q bank balances pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj --node tcp://localhost:26657 -o json | grep -o '"amount":"[0-9]*"'
```

**Expect**: `"amount":"999999999998"`. Write the number down.

## Step 10: send relays

**Run**

```bash
./bin/pocket-relay-miner relay jsonrpc --localnet --service develop-http; echo "EXIT=$?"
```

**Expect**

```
Supplier: pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
...
Status: ✅ SUCCESS
Signature: ✅ VALID
Error Check: ✅ NO ERRORS
...
EXIT=0
```

The `Supplier:` line names the supplier that served it; use that address in
steps 11 and 12 if it is not supplier 1. Then send a few hundred more so the
claim is not trivial:

```bash
./bin/pocket-relay-miner relay jsonrpc --localnet --service develop-http --load-test --count 300 --concurrency 10; echo "EXIT=$?"
```

**Expect**: `Successful: 300`, `Errors: 0`, `Success Rate: 100.00%`, `EXIT=0`.

**If not**:
- `connection refused` on `localhost:8180` → the relayer is not published →
  step 4.
- HTTP 503 → the relayer is not ready → step 7.
- `Signature: ❌` → relayer and client disagree on keys → check that
  `examples/docker-compose/config/supplier-keys.yaml` is unmodified.

## Step 11: the claim and the proof land on chain

A session on this chain is 20 blocks. A relay sent at height H belongs to the
session that ends at the next multiple of 20. Its claim lands about 11 to 21
blocks after that end, and the proof about 11 blocks later. At ~11 seconds per
block, expect the claim 4 to 6 minutes after step 10 and settlement about
4 minutes after that.

**Run** (repeat every minute until it shows a claim)

```bash
$C exec validator pocketd q proof list-claims --node tcp://localhost:26657 -o json
```

**Expect**: a claim for your supplier and `develop-http`, first
`PENDING_VALIDATION`, then `VALIDATED` once the proof is in. From the real run
(relays at height 10, claim seen at height 33, validated at height 44):

```
{"claims":[{"supplier_operator_address":"pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj","session_header":{"application_address":"pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4","service_id":"develop-http", ... "session_end_block_height":"20"}, ... "proof_validation_status":"VALIDATED"}], ...}
```

The claim disappears from this list once it settles, so `"claims":[]` after
the window closes does not mean it never landed: check the miner's record.

**Run**

```bash
$C exec miner pocket-relay-miner redis --config /config/miner.yaml submissions --supplier pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
```

**Expect**

```
SESSION_END  SERVICE       CLAIM_STATUS  PROOF_STATUS  RELAYS  CU      SESSION_ID
-----------  -------       ------------  ------------  ------  --      ----------
20           develop-http  ✓ SUCCESS     ✓ SUCCESS     301     301000  fce30d03f194...
```

**If not**: no row after 30 blocks past the session end → the miner did not
submit → `$C logs miner | grep '"level":"error"'`.

**Stop if**: a claim or proof shows `FAILED`; report the row and the miner's
error lines.

## Step 12: the reward is settled

**Run** (after the claim reached `VALIDATED` and a few more blocks passed)

```bash
$C exec validator pocketd q bank balances pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj --node tcp://localhost:26657 -o json | grep -o '"amount":"[0-9]*"'
```

**Expect**: above the step 9 baseline. The real run went from `999999999998`
to `999999999997` (claim fee), `999998999996` (proof fee, 1 POKT) and then
`1000020069996` at settlement: +21070000 upokt net of fees.

This is the end of the local deployment: relays served, claimed, proved and
paid.

## Reset

**Run**

```bash
$C down -v; echo "EXIT=$?"
```

**Expect**: `Volume prm-example_validator-data Removed`,
`Volume prm-example_redis-data Removed`, `EXIT=0`.

`-v` deletes the chain and Redis together. Keep them together: a Redis holding
sessions of a previous chain does not match a new one.

## Pointing it at your own node

For a real network, keep the relayer, miner and Redis services and drop the
local chain. Do each change, then run step 2 (validate) again.

**Stop if**: you do not have a staked supplier key and a funded account from a
human. Never use the keys in `examples/docker-compose/` on a real network: they
are public.

1. **Remove the local chain**: delete the `validator` and `account-init`
   services, and the `validator` and `account-init` entries under
   `depends_on:` of `miner`. Delete the `validator-data` volume and the
   `localnet/` mount. Keep `backend` only if you want the test backend.
2. **Point both configs at your node**, in `config/relayer.yaml` and
   `config/miner.yaml`:
   - `pocket_node.query_node_rpc_url`: your node's CometBFT RPC URL.
   - `pocket_node.query_node_grpc_url`: your node's gRPC, as `host:port`.
   - `pocket_node.grpc_insecure`: `false` for a TLS endpoint (`:443`).
3. **Set the network in the miner config**:
   - `pocket_node.chain_id`: `pocket` for mainnet, `pocket-beta` for the testnet.
   - `block_time_seconds`: the measured block time of that network (mainnet is
     roughly 60).
4. **Use your keys**: replace `config/supplier-keys.yaml` with a file that
   holds only your suppliers' keys (same `keys:` format), outside version
   control, mode 0600. For a keyring instead, see
   [docs/SUPPLIER_KEYS.md](../SUPPLIER_KEYS.md).
5. **Declare your services** in `config/relayer.yaml` under `services.<service_id>`,
   1 entry per service your suppliers are staked for, each with its
   `backends.<transport>.url`. [config.relayer.example.yaml](../../config.relayer.example.yaml)
   documents every option.
6. **Size Redis and the containers**: raise `--maxmemory` in the `redis`
   service and every `mem_limit`, keeping `mem_limit` of Redis at least 1.25x
   its `maxmemory`, and `GOMEMLIMIT` about 10% below each process's
   `mem_limit`. [config.redis.example.conf](../../config.redis.example.conf)
   has the values v0.1.0 was measured with.
7. **Publish the relay port** for your gateways: the `relayer` service's
   `ports:` binds `127.0.0.1` only; bind the address your gateways reach.
   Never publish Redis.
8. **Check the stake against your backends**, once the node is reachable:

   ```bash
   $C run --rm --no-deps relayer relayer validate --config /config/relayer.yaml --check-stake; echo "EXIT=$?"
   ```

   **Expect**: `EXIT=0`. A line `staked but no backend: supplier=... service=... transport=...`
   is a staked service you do not serve: add it, or stop and ask.

Then start with step 3 and check steps 4, 6 and 7. To test the relayer without
a staked application, use a simulated relay:
[docs/SIMULATED_RELAYS.md](../SIMULATED_RELAYS.md). Pointing at a real network
is not verified end to end by this runbook.
