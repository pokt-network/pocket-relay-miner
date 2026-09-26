# Deploy on a host (binary + systemd)

This runbook installs the relayer and the miner as 2 systemd services on 1
Linux host (or VM), next to Redis 8.10 or newer. Example files are in
[examples/host/](../../examples/host/).

**Verification status.** The configs in `examples/host/` pass `validate`, and
both units pass `systemd-analyze verify` (systemd 255). The binaries' startup
checks quoted here (Redis settings, chain, `/ready`) were run on a host
without systemd. The full sequence under systemd against a real network is
**not verified end to end**. For a verified end-to-end run, see
[DOCKER_COMPOSE.md](DOCKER_COMPOSE.md).

How to read a step: **Run**, compare with **Expect**, use **If not** when it
differs, and **Stop if** says when to ask a human. Error messages are explained
in [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Step 0: what you need

- A Linux host with systemd and cgroup v2, root access, and the
  [prerequisites](README.md#prerequisites): a reachable node, a staked
  supplier key, a backend per service, a funded supplier account.
- A copy of this repository at tag `v0.1.0`, for `examples/host/` and
  `config.redis.example.conf`:

```bash
git clone --branch v0.1.0 https://github.com/pokt-network/pocket-relay-miner.git && cd pocket-relay-miner
```

**If not**: `Remote branch v0.1.0 not found in upstream origin` → the release
is not published yet → clone without `--branch`, and in step 1 build from
source: the release image does not exist either.

**Stop if**: you must run a published release and v0.1.0 is not out yet.

**Stop if**: you do not have the supplier key or the node endpoints. A human
provides them; do not generate keys or move funds.

## Step 1: get the binary

Pick 1 of 2 ways. Both give the same binary; relayer and miner must use the
same one.

**Run** (build from source; needs Go 1.26.5 and make)

```bash
make build-release
```

**Expect**: `Release build complete: bin/pocket-relay-miner`

Or **Run** (copy it out of the release image; needs Docker, and the binary is
statically linked)

```bash
mkdir -p bin && id=$(docker create ghcr.io/pokt-network/pocket-relay-miner:v0.1.0) && docker cp "$id:/usr/local/bin/pocket-relay-miner" bin/pocket-relay-miner && docker rm "$id"
```

Then install it and create the service user:

```bash
sudo install -m 0755 bin/pocket-relay-miner /usr/local/bin/pocket-relay-miner
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pocket-relay-miner
sudo install -d -m 0750 -o root -g pocket-relay-miner /etc/pocket-relay-miner
```

**Run**

```bash
/usr/local/bin/pocket-relay-miner version
```

**Expect**: a line starting with `pocket-relay-miner version`.

## Step 2: Redis 8.10

Redis 8.10 or newer is required. Redis must load
[config.redis.example.conf](../../config.redis.example.conf): both binaries
refuse to start when `redis_version` is below 8.10, when `maxmemory` is 0, or
when `maxmemory-policy` is anything other than `noeviction`. `validate` does not
connect to Redis; these are checked when the process starts.

Distribution packages may ship an older Redis; check before installing. Either
use the Redis project's packages for 8.10, or run the official image on the
host network, bound to `127.0.0.1`:

```bash
sudo install -d /etc/redis && sudo install -m 0644 config.redis.example.conf /etc/redis/relay-miner.conf
docker run -d --name relay-miner-redis --restart unless-stopped -p 127.0.0.1:6379:6379 -v /etc/redis/relay-miner.conf:/usr/local/etc/redis/redis.conf:ro -v relay-miner-redis-data:/data redis:8.10.1-alpine redis-server /usr/local/etc/redis/redis.conf --maxmemory 4gb
```

With a packaged Redis, add `include /etc/redis/relay-miner.conf` at the end
of its `redis.conf` and restart it. The included file sets
`maxmemory 13743895347` (12.8 GiB), and being last it wins over `redis.conf`:
edit that line in `/etc/redis/relay-miner.conf` to fit this host, as sized
below, before the restart.

Size `maxmemory` for your sessions and leave headroom below the RAM Redis may
use: the example conf's 12.8 GiB was measured in a 16 GiB container. The
`--maxmemory 4gb` above is an example; change it.

**Run**

```bash
redis-cli --raw -h 127.0.0.1 INFO server | grep redis_version; redis-cli --raw -h 127.0.0.1 CONFIG GET maxmemory-policy
```

**Expect**

```
redis_version:8.10.1
maxmemory-policy
noeviction
```

No `redis-cli` on the host: with the container above, run the same commands
as `docker exec relay-miner-redis redis-cli --raw CONFIG GET maxmemory-policy`
(and likewise for `INFO server`).

**If not**: another policy, or `CONFIG GET maxmemory` returns `0` → the conf
was not loaded → see [Redis](TROUBLESHOOTING.md#redis).

**Stop if**: the host's Redis is shared with other applications. Redis holds
every claim tree here; give the relay miner its own instance.

Never expose Redis beyond `127.0.0.1` or a private network.

## Step 3: configs and keys

**Run**

```bash
sudo install -m 0640 -g pocket-relay-miner examples/host/relayer.yaml examples/host/miner.yaml /etc/pocket-relay-miner/
sudo install -m 0600 -o pocket-relay-miner examples/host/supplier-keys.yaml.example /etc/pocket-relay-miner/supplier-keys.yaml
sudo install -m 0600 -o pocket-relay-miner examples/host/pocket-relay-miner.env.example /etc/pocket-relay-miner/relayer.env
sudo install -m 0600 -o pocket-relay-miner examples/host/pocket-relay-miner.env.example /etc/pocket-relay-miner/miner.env
```

Then edit every line marked `CHANGE`:

- `/etc/pocket-relay-miner/relayer.yaml` and `miner.yaml`:
  `pocket_node.query_node_rpc_url`, `pocket_node.query_node_grpc_url`,
  `pocket_node.grpc_insecure`.
- `miner.yaml`: `pocket_node.chain_id` (`pocket` for mainnet, `pocket-lego-testnet`
  for the beta testnet) and `block_time_seconds` (measured; beta is roughly 30,
  mainnet roughly 60).
- `relayer.yaml`: `services.<service_id>` with `backends.<transport>.url`, 1
  entry per service your suppliers are staked for.
- `supplier-keys.yaml`: your suppliers' private keys, 1 per line under `keys:`.
  For a keyring instead, see [docs/SUPPLIER_KEYS.md](../SUPPLIER_KEYS.md) and
  set `KEYRING_PASSPHRASE` in both `.env` files.
- `miner.env`: `GOMEMLIMIT=7200MiB` (the miner unit has `MemoryMax=8G`).
- pprof: the relayer example serves it on `127.0.0.1:6060`; the miner example
  leaves it off. Both binaries default to `127.0.0.1:6060`, so if you enable
  it in `miner.yaml`, give it another `pprof.addr` (for example
  `127.0.0.1:6065`) or the 2 processes on this host collide.

[config.relayer.example.yaml](../../config.relayer.example.yaml) and
[config.miner.example.yaml](../../config.miner.example.yaml) document every
other key.

**Stop if**: you are about to paste a private key into a file that is under
version control or readable by other users.

## Step 4: validate both configs

**Run**

```bash
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner relayer validate --config /etc/pocket-relay-miner/relayer.yaml; echo "EXIT=$?"
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner miner validate --config /etc/pocket-relay-miner/miner.yaml; echo "EXIT=$?"
```

**Expect**

```
config OK: /etc/pocket-relay-miner/relayer.yaml would start
EXIT=0
config OK: /etc/pocket-relay-miner/miner.yaml would start
EXIT=0
```

**If not**: `Error: config is INVALID: ...` → it names the key (and the line, for an
unknown or retired key) → fix it and validate again. See [Config rejected](TROUBLESHOOTING.md#config-rejected).

`validate` does not open the keys file, Redis or the chain. Then check that
every staked service has a backend (this one queries the chain):

**Run**

```bash
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner relayer validate --config /etc/pocket-relay-miner/relayer.yaml --check-stake; echo "EXIT=$?"
```

**Expect**: `EXIT=0`.

**If not**: `ERROR  staked but no backend: supplier=... service=... transport=...`
→ a staked service is not in `services` → add it.

**Stop if**: `--check-stake` lists a supplier you did not expect: the keys file
may hold the wrong keys.

## Step 5: install the units

The units set `MemoryMax` and `CPUQuota` (hard limits) and read `GOMEMLIMIT`
and `GOMAXPROCS` from the `.env` files. Each runs `validate` as
`ExecStartPre`, so a config that does not validate never starts.

**Run**

```bash
sudo install -m 0644 examples/host/pocket-relay-miner-miner.service examples/host/pocket-relay-miner-relayer.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemd-analyze verify /etc/systemd/system/pocket-relay-miner-miner.service /etc/systemd/system/pocket-relay-miner-relayer.service; echo "EXIT=$?"
```

**Expect**: no output and `EXIT=0`. `systemd-analyze verify` also exits 0 on
an unknown key, so any printed line is a problem to fix.

If Redis runs as a packaged service not named `redis-server.service`, change
`After=` in both units.

## Step 6: start the miner

**Run**

```bash
sudo systemctl enable --now pocket-relay-miner-miner
sleep 20; systemctl is-active pocket-relay-miner-miner; curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:9092/health
```

**Expect** (not verified under systemd; the health body is from the binary):

```
active
OK HTTP=200
```

**If not**: `activating` or `failed` → read why:

```bash
journalctl -u pocket-relay-miner-miner -n 50 --no-pager | grep -i 'error'
```

The common ones: `cannot start without the node's network`,
`the node reports network "..." and this miner is configured for chain "..."`,
`redis maxmemory is 0`, `redis maxmemory-policy is "..."`. Each is in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md).

**Run** (check the memory limit took effect)

```bash
journalctl -u pocket-relay-miner-miner --no-pager | grep -m1 'process memory limit set'
```

**Expect**: a line with `"source":"env"` and `"limit_bytes":7549747200`
(7200MiB). `no memory limit found` means `GOMEMLIMIT` is not set and no cgroup
limit was found: fix the `.env` file.

**Stop if**: the miner restarts in a loop for more than 5 minutes.

## Step 7: start the relayer

**Run**

```bash
sudo systemctl enable --now pocket-relay-miner-relayer
sleep 20; curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:8081/ready
```

**Expect**: `READY HTTP=200`

**If not**: `no service factor manifest: the miner has not published one yet HTTP=503`
→ the miner is not running or has not published yet → step 6. See
[Relayer up but not ready](TROUBLESHOOTING.md#relayer-up-but-not-ready).

## Step 8: serve a relay

On a real network you need a staked application or a gateway to send a real
relay. Without one, send a simulated relay: real signature, real backend
call, never charged or claimed. Set it up with
[docs/SIMULATED_RELAYS.md](../SIMULATED_RELAYS.md). The relayer reads the
simulation settings only at startup, so after editing
`/etc/pocket-relay-miner/relayer.yaml` run step 4's relayer `validate` again
(`EXIT=0`), then `sudo systemctl restart pocket-relay-miner-relayer` and wait
for `/ready` as in step 7. Then, with `<sim-keys-file>` the absolute path of
the simulation keys file you created and `<chain-id>` the miner's
`pocket_node.chain_id` (placeholders, like every `<...>` here):

```bash
pocket-relay-miner relay jsonrpc --relayer-url http://127.0.0.1:8080 --node <node-grpc-host:port> --grpc-tls --chain-id <chain-id> --keys-file <sim-keys-file> --service <service-id> --supplier <supplier-address> --simulate --sim-key-id <identity>; echo "EXIT=$?"
```

**Expect**: `Status: ✅ SUCCESS`, `Signature: ✅ VALID`, `EXIT=0`.

Real relays with the CLI: [docs/testing/DIRECT_CLI.md](../testing/DIRECT_CLI.md).

## Step 9: watch claims and proofs

After a session with served relays ends, its claim and then its proof are
submitted in the chain's windows.

**Run**

```bash
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner redis --config /etc/pocket-relay-miner/miner.yaml submissions --supplier <supplier-address>
```

**Expect**: 1 row per claimed session with `CLAIM_STATUS` and `PROOF_STATUS`
at `✓ SUCCESS` once the proof window has passed.

**Stop if**: any row shows a failure; report it with the miner's error lines.

## Operating

- **Config changes need a restart**: `sudo systemctl restart pocket-relay-miner-relayer`.
  The relayer reads its config once; only supplier keys reload while running.
- **Upgrade both together**: install the new binary, validate both configs with
  it, then restart the miner and the relayer.
- **Logs**: `journalctl -u pocket-relay-miner-relayer -f`. Per-relay rejections
  log at debug level; count them with the metric
  `ha_relayer_relays_rejected_total` on `127.0.0.1:9090/metrics`.
