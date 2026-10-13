# Deploy on a host (binary + systemd): standalone mode

This runbook installs the **standalone mode** as 1 systemd service on 1 Linux
host (or VM): the relayer and the miner in 1 process, their state in an
embedded store under `/var/lib/pocket-relay-miner`, and **no Redis**. Example
files are in [examples/host/](../../examples/host/): `standalone.yaml` and
`pocket-relay-miner-standalone.service`.

For the **high-availability mode** (Redis, a relayer and a miner as 2
services), follow [HOST.md](HOST.md) instead.
[README.md, "Choose a mode"](README.md#choose-a-mode) compares the two. Never
run both modes for the same supplier keys.

**Verification status.** `examples/host/standalone.yaml` passes `standalone
validate`, and the unit passes `systemd-analyze verify` (systemd 255, with the
binary's path pointed at a local build). The process was started from this
repository on 2026-10-07 without a chain: it opened its store, connected to no
Redis, and stopped where it needs the node. The sequence under systemd against
a real network is **not verified end to end**.

How to read a step: **Run**, compare with **Expect**, use **If not** when it
differs, and **Stop if** says when to ask a human. Error messages are explained
in [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

## Step 0: what you need

- A Linux host with systemd and cgroup v2, root access, and the
  [prerequisites](README.md#prerequisites) of standalone mode: a reachable
  node, a staked supplier key, a backend per service, a funded supplier
  account, and free space on the disk of `/var/lib`.
- A copy of this repository, for `examples/host/`. Standalone mode is not in
  `v0.1.2`: use a checkout that has `docs/deploy/HOST_STANDALONE.md` (this
  file).

**Run**

```bash
df -h /var/lib
```

**Expect**: several GiB available. **Stop if**: less than 2 GiB: the process
takes no new work below 1 GiB free.

## Step 1: get the binary

Standalone mode is not in a release yet: build it from this checkout (needs Go
1.26.5 and make).

**Run**

```bash
make build-release
```

**Expect**: `Release build complete: bin/pocket-relay-miner`

Then install it and create the service user:

```bash
sudo install -m 0755 bin/pocket-relay-miner /usr/local/bin/pocket-relay-miner
sudo useradd --system --no-create-home --shell /usr/sbin/nologin pocket-relay-miner
sudo install -d -m 0750 -o root -g pocket-relay-miner /etc/pocket-relay-miner
```

**Run**

```bash
/usr/local/bin/pocket-relay-miner standalone --help | head -2
```

**Expect**: `Run the relayer and the miner in one process, from one config
file, with` (the first line of the command's description).

## Step 2: no Redis

Standalone mode keeps its state in the embedded store; there is nothing to
install for it. The unit's `StateDirectory=` creates
`/var/lib/pocket-relay-miner` for the service user when it first starts, and
`storage.path` in the config is below it.

## Step 3: config and keys

**Run**

```bash
sudo install -m 0640 -g pocket-relay-miner examples/host/standalone.yaml /etc/pocket-relay-miner/
sudo install -m 0600 -o pocket-relay-miner examples/host/supplier-keys.yaml.example /etc/pocket-relay-miner/supplier-keys.yaml
sudo install -m 0600 -o pocket-relay-miner examples/host/pocket-relay-miner.env.example /etc/pocket-relay-miner/standalone.env
```

The config points at the beta testnet through the public Sauron endpoints;
every network value has its mainnet value in a comment marked `Mainnet:`. Then
edit every line marked `CHANGE`:

- `standalone.yaml`, under `relayer:` → `services:`: 1 entry per service your
  suppliers are staked for, with `backends.<transport>.url`. Keep an active
  `health_check` on every HTTP backend and change its probe to one your node
  answers ([why](README.md#backend-health-checks-turn-them-on)). Check each
  backend answers from this host before starting (`curl` the same probe).
- `supplier-keys.yaml`: your suppliers' private keys, as in
  [HOST.md, step 3](HOST.md#step-3-configs-and-keys). The human who owns the
  supplier creates and stakes it with `pocketd`.
- `standalone.env`: `GOMEMLIMIT=10800MiB` (the unit has `MemoryMax=12G`).

[docs/STANDALONE.md](../STANDALONE.md) describes the config file;
[config.standalone.example.yaml](../../config.standalone.example.yaml)
annotates it.

**Stop if**: you are about to paste a private key into a file that is under
version control or readable by other users, or you do not have a staked
supplier's key from a human. An agent never generates, funds or stakes a key.

## Step 4: validate the config

**Run**

```bash
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner standalone validate --config /etc/pocket-relay-miner/standalone.yaml; echo "EXIT=$?"
```

**Expect** (checked against `examples/host/standalone.yaml`)

```
config OK: /etc/pocket-relay-miner/standalone.yaml would start
EXIT=0
```

**If not**: `Error: config is INVALID: ...` → it names the key (and the line,
for an unknown key) → fix it and validate again. `a standalone config has no
redis section` → remove the `redis:` section: standalone mode uses no Redis.

Then check that every staked service has a backend (this queries the chain):

**Run**

```bash
sudo -u pocket-relay-miner /usr/local/bin/pocket-relay-miner standalone validate --config /etc/pocket-relay-miner/standalone.yaml --check-stake; echo "EXIT=$?"
```

**Expect**: `EXIT=0`. **If not**: `ERROR  staked but no backend: ...` → a
staked service is not under `relayer.services` → add it.

## Step 5: install the unit

**Run**

```bash
sudo install -m 0644 examples/host/pocket-relay-miner-standalone.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemd-analyze verify /etc/systemd/system/pocket-relay-miner-standalone.service; echo "EXIT=$?"
```

**Expect**: no output and `EXIT=0` (checked with the binary's path pointed at a
local build).

## Step 6: start it

**Run**

```bash
sudo systemctl enable --now pocket-relay-miner-standalone
sleep 30; systemctl is-active pocket-relay-miner-standalone
curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:9092/health
curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:8081/ready
```

**Expect** (not verified under systemd)

```
active
OK HTTP=200
READY HTTP=200
```

**If not**: `activating` or `failed` → read why:

```bash
journalctl -u pocket-relay-miner-standalone -n 50 --no-pager | grep -i 'error'
```

The common ones: `cannot start without the node's network`,
`the node reports network "..." and this miner is configured for chain "..."`,
and, for the store, a path the service user cannot write. Each is in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md). `READY` 503 with `no service factor
manifest` → the miner side has not published yet: wait a minute.

**Run** (the store has room)

```bash
curl -s http://127.0.0.1:9092/metrics | grep -E '^ha_transport_store_(free_bytes|operable)'
```

**Expect** (not verified): both `ha_transport_store_operable` series at `1`.

**Stop if**: the process restarts in a loop for more than 5 minutes.

## Step 7: the relayer side receives blocks

**Run**

```bash
curl -s http://127.0.0.1:9092/metrics | grep -E '^ha_relayer_current_block_height'
```

Run it again a minute later. **Expect**: the height grows (both sides' metrics
are on 9092).

## Step 8: serve a relay

As in [HOST.md, step 8](HOST.md#step-8-serve-a-relay): a simulated relay to
`http://127.0.0.1:8080`. The simulation settings go under `relayer:` in
`standalone.yaml`; after editing it, validate (step 4) and
`sudo systemctl restart pocket-relay-miner-standalone`.

## Step 9: watch claims and proofs

**Run** (every few minutes, once a session with served relays has ended)

```bash
curl -s http://127.0.0.1:9092/metrics | grep -E '^ha_miner_(claims_submitted|claim_errors|proofs_submitted|proof_errors)_total'
```

**Expect**: `ha_miner_claims_submitted_total`, then
`ha_miner_proofs_submitted_total`, grow; the `_errors_total` series stay absent
or flat. The `pocket-relay-miner redis ...` inspection commands of the
high-availability runbook read Redis; standalone mode has none. **Not
verified** on beta.

**Stop if**: an `_errors_total` series grows; report it with the error lines of
`journalctl -u pocket-relay-miner-standalone`.

## Dashboards (optional)

Scrape `127.0.0.1:9092` once, as job `standalone` (scraped under 2 jobs,
every summed panel would count it twice), as
[examples/observability/prometheus/prometheus.standalone.yml](../../examples/observability/prometheus/prometheus.standalone.yml)
does for the compose example. **Not verified**.

## Switching to mainnet

**Stop if**: you have not been told by a human to run on mainnet.

In `/etc/pocket-relay-miner/standalone.yaml`, switch every value marked
`Mainnet:` (`pocket_node.query_node_rpc_url`, `pocket_node.query_node_grpc_url`,
`pocket_node.chain_id`, `miner.block_time_seconds`), validate (step 4) and
restart the unit.

## Operating

- **Config changes need a restart**:
  `sudo systemctl restart pocket-relay-miner-standalone`. Only supplier keys
  reload while running.
- **Stopping**: the relayer side drains first, then the miner side; the unit
  gives it 120 s (`TimeoutStopSec`).
- **The store**: `/var/lib/pocket-relay-miner`. Deleting it deletes the relays
  not yet claimed and the trees of sessions not yet proved: never while a
  claimed session awaits its proof.
- **Logs**: `journalctl -u pocket-relay-miner-standalone -f`.
