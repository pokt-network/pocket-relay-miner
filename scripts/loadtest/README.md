# Loadtest — direct backend RPS ceiling

Measures the sustained RPS of each upstream RPC backend **without going
through the relayer**, finds the optimal concurrency under a p99
latency budget, and produces a `service → max_conns` table you can map
straight onto the relayer's per-service pool config.

## What it's for

1. **Know how much each backend can deliver** — `sweep` mode
2. **Find the best `max_conns_per_host` per chain** under a latency
   budget — `sweep-optimal` mode
3. **Detect dead or degraded backends** — `probe` mode

The script ships **no infrastructure data**. Backend URLs and the
ssh host live in a config file that you create under `localonly/`
(gitignored). Any operator can use the script against their own
backends by writing their own conf.

## Setup

```bash
# 1. Create your conf from the example
mkdir -p scripts/localonly/loadtest
cp scripts/loadtest/backends.conf.example scripts/localonly/loadtest/backends.conf

# 2. Edit it: set SSH_HOST and SERVICE_URL entries pointing at your nodes
$EDITOR scripts/localonly/loadtest/backends.conf

# 3. Verify everything is reachable (one curl per backend)
scripts/loadtest/backends.sh probe
```

**`localonly/` is gitignored**, so your real URLs never get committed.
If you'd rather keep your conf elsewhere:

```bash
BACKENDS_CONF=/some/other/path/backends.conf scripts/loadtest/backends.sh probe
```

## How it works

- **Local or remote execution.** If `SSH_HOST=""` in your conf, the
  script runs `hey` directly on the machine where you invoke it. If
  `SSH_HOST=somehost`, it sshes there and runs `hey` from a docker
  container with `--network host` (handy when backends are
  IP-whitelisted to a specific bastion / relay host).
- **Concurrency model.** Instead of asking for a target RPS (which
  `hey -q` applies per-worker, leading to confusing math), we tell
  hey "send N requests with C concurrent workers as fast as you
  can" and measure the resulting RPS. Honest and reproducible.
- **`sweep`** iterates concurrency `10 50 100 250 500 1000` per
  service and reports the best healthy RPS.
- **`sweep-optimal`** does the same but discards any run whose p99
  exceeds `MAX_P99_MS` (default 200). It reports the concurrency
  with the highest RPS *within* the budget — the value you want as
  your pool cap.

## Commands

```bash
# List configured services and their known status
scripts/loadtest/backends.sh list

# Probe: 1 manual curl per backend (no hey, no docker)
scripts/loadtest/backends.sh probe

# Single test against eth: 2000 reqs, 100 concurrent workers
scripts/loadtest/backends.sh test eth --reqs 2000 --conns 100

# Ramp: walk concurrency 10 → 1000 against sui, report max healthy RPS
scripts/loadtest/backends.sh ramp sui

# Optimal: like ramp, but filters runs exceeding p99 ≤ 100ms.
# Reports the best concurrency that meets the budget.
DEFAULT_REQS=20000 \
  scripts/loadtest/backends.sh optimal eth --max-p99-ms 100

# Full sweep (raw max RPS) over healthy services
scripts/loadtest/backends.sh sweep > /tmp/sweep.csv 2> /tmp/sweep.log

# Sweep-optimal: per-service recommended max_conns under a latency budget.
# The summary at the end of stderr is a table service/max_conns/rps/p99
# directly mappable to a per-service pool_profile in the relayer config.
DEFAULT_REQS=20000 MAX_P99_MS=100 \
  scripts/loadtest/backends.sh sweep-optimal \
  > /tmp/optimal.csv 2> /tmp/optimal.log
tail -20 /tmp/optimal.log   # see the recommended table

# Force inclusion of services marked broken in the conf
scripts/loadtest/backends.sh sweep --include-broken > /tmp/sweep-all.csv
```

## Payload presets (critical for correct pool sizing)

The default `light` preset sends a minimal `eth_blockNumber`-shaped
request (~70 bytes). That's fine for probe/ceiling but **wildly
underestimates the cost of real production traffic** — on breeze we
measured sui/op with p95 request bodies of 7-9 KB, 100x the light
preset. Pool tuning with light payloads will under-provision pools
for services whose real traffic is batch-heavy or uses large methods.

```bash
# Default: light ping (misleading for production tuning)
scripts/loadtest/backends.sh optimal sui

# Heavy: per-chain representative payload (JSON-RPC batch for EVM,
# sui_multiGetObjects for sui). Use this when tuning for real traffic.
scripts/loadtest/backends.sh optimal sui --payload-preset heavy

# Custom: drop in a recorded payload from your production logs.
# File contents are sent verbatim as the request body.
scripts/loadtest/backends.sh optimal eth \
  --payload-file scripts/localonly/loadtest/payloads/eth-getlogs.json

# Sweep with the heavy preset so all services get a representative test
DEFAULT_REQS=20000 MAX_P99_MS=100 \
  scripts/loadtest/backends.sh sweep-optimal --payload-preset heavy \
  > /tmp/optimal-heavy.csv 2> /tmp/optimal-heavy.log
```

Every CSV row includes `payload_bytes` and `payload_tag` so a run is
self-describing when you compare light vs heavy sweeps side by side.

Known limits: `--payload-file` passes the body as a docker argv to
`hey`, so payloads much larger than ~100 KB may hit ARG_MAX. For
operator-specific payloads keep `scripts/localonly/loadtest/payloads/`
(gitignored) alongside `backends.conf` so real method parameters never
get committed.

## Recipes

### I'm tuning pools for the first time

```bash
# 1. Confirm every backend responds
scripts/loadtest/backends.sh probe

# 2. Find the optimal concurrency for each, p99 ≤ 100ms,
#    sustained sample (20k reqs per test, ~10 min total)
DEFAULT_REQS=20000 MAX_P99_MS=100 \
  scripts/loadtest/backends.sh sweep-optimal \
  > /tmp/optimal.csv 2> /tmp/optimal.log

# 3. Read the table at the end of the log
tail -25 /tmp/optimal.log
```

The table has one row per service: `service max_conns rps p99_ms`.
Map each row to a `pool_profile` (low/medium/high) by `max_conns`,
then assign the profile to the service in your relayer config.

### My eth backend changed, I want to re-measure just that

```bash
DEFAULT_REQS=20000 \
  scripts/loadtest/backends.sh optimal eth --max-p99-ms 100
```

### How sensitive is the answer to my latency budget?

```bash
for budget in 50 100 150 200; do
  echo "=== p99 ≤ ${budget}ms"
  DEFAULT_REQS=20000 MAX_P99_MS=$budget \
    scripts/loadtest/backends.sh optimal poly --max-p99-ms $budget \
    2>&1 | grep OPTIMAL
done
```

Fast services (sub-millisecond backend latency) will give the same
answer regardless of budget. Slow ones will move a lot — that tells
you how exposed they are to the budget choice.

## CSV columns

`service,backend,conns,reqs_target,total_secs,rps,reqs,success_pct,p50_ms,p95_ms,p99_ms`

## Environment variables

| var                   | default                  | notes                                       |
|-----------------------|--------------------------|---------------------------------------------|
| `BACKENDS_CONF`       | `scripts/localonly/loadtest/backends.conf` | path to your conf file        |
| `SSH_HOST`            | (from conf)              | `""` runs locally, otherwise sshes there    |
| `HEY_IMAGE`           | `williamyeh/hey`         | docker image for hey                        |
| `DEFAULT_REQS`        | `2000`                   | requests per test (raise to 20000 sustained)|
| `DEFAULT_CONNS`       | `100`                    | concurrency for `test` mode                 |
| `RAMP_CONNS`          | `10 50 100 250 500 1000` | concurrency steps for ramp/optimal          |
| `MAX_P99_MS`          | `200`                    | p99 budget for `optimal`/`sweep-optimal`    |
| `FAIL_ON_P99_MS`      | `2000`                   | hard stop threshold for ramp/sweep          |
| `FAIL_ON_SUCCESS_PCT` | `98`                     | minimum success%                            |

## Tuning is per-replica

The script measures **one** client saturating one backend. The
relayer in production also acts as one client per replica — its
HTTP pool serves all the gateway requests funnelling through that
process. So the concurrency the script returns **is** the
`max_conns_per_host` value one relayer replica should use.

What scales is the **number of replicas**, not the per-replica pool
size. If you scale to 3 replicas, each one still uses the same value
the script gave you, and the combined ceiling at the backend is 3×.

The values are operator-specific. Different operators with different
upstream nodes will get different recommendations. That's the point —
run the script against your own infrastructure to get the answer that
fits your topology.

## Limitations

- **Competes with real production traffic.** Run outside peak hours
  or coordinate with the operator.
- **HTTP/JSON-RPC only.** WebSocket, gRPC, and REST backends need
  different tooling. Most relay traffic is JSON-RPC, so this covers
  what matters for pool tuning.
- **Single client.** Production has many gateways funnelling through
  the relayer; this script measures what one client (= one relayer
  replica) can extract from the backend. That's the right answer for
  per-replica pool sizing — see the section above.
- **Why not test through the relayer?** The relayer expects a fully
  signed `RelayRequest` proto with a ring signature from an
  application with an active session. You can't send "raw" relays
  from outside. Testing the full path (client → PATH → relayer →
  backend) needs a PATH gateway with a real app stake. That's a
  separate project. In the meantime, the gap between the numbers
  here and the throughput visible in `ha_relayer_*` metrics tells
  you what the relayer is costing.
