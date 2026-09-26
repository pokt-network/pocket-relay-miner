# Troubleshooting a deployment

Find the message you see, then apply the action. Every message below is copied
from the v0.1.0 binary: from a real run, or from the source where noted.

Compose users: read logs with
`docker compose -p prm-example -f examples/docker-compose/docker-compose.yaml logs <service>`.
Host users: `journalctl -u pocket-relay-miner-<relayer|miner> -n 100 --no-pager`.
Logs are JSON by default; search for `"level":"error"`, `"level":"warn"` and `Error:`.

## Quick table

| Symptom (grep for it) | Cause | Action |
|---|---|---|
| `Error: config is INVALID` | a key is unknown, retired, or has a bad value | [Config rejected](#config-rejected) |
| `redis maxmemory is 0 (no memory limit)` | Redis has no `maxmemory` | [Redis](#redis) |
| `redis maxmemory-policy is "allkeys-lru": set it to noeviction` | Redis evicts keys | [Redis](#redis) |
| `cannot start without the node's network` | the miner cannot reach the node's gRPC | [Miner does not start](#miner-does-not-start) |
| `the node reports network "..." and this miner is configured for chain "..."` | `pocket_node.chain_id` does not match the node | [Miner does not start](#miner-does-not-start) |
| `block_time_seconds is required and must be positive` | `block_time_seconds` missing from the miner config | [Miner does not start](#miner-does-not-start) |
| `/ready` answers `no service factor manifest: the miner has not published one yet` (503) | the miner is not running, or has not published yet | [Relayer up but not ready](#relayer-up-but-not-ready) |
| `no memory limit found: the process runs without one` | no `GOMEMLIMIT` and no container / cgroup limit | [Memory and CPU](#memory-and-cpu) |
| `maxprocs: Leaving GOMAXPROCS=<host cores>: CPU quota undefined` | no `GOMAXPROCS` and no CPU limit | [Memory and CPU](#memory-and-cpu) |
| relays answered 503 `relayer is not admitting relays right now` | not priced yet, or the publish queue is full | [Relays refused](#relays-refused) |
| relays answered 429 `relayer is not admitting relays: storage saturated` | Redis is nearly full | [Relays refused](#relays-refused) |

## Config rejected

`validate` reports every unknown or retired key in 1 pass, each with its line
number. A key it knows but whose value it rejects is reported alone, the first
one found, without a line number: fix it and run `validate` again until it
exits 0.

A retired key names what replaced it. Real output (a relayer config still
carrying `relay_meter.fail_behavior`):

```
Error: config is INVALID: 1 key(s) this relayer does not understand:
  line 60: field fail_behavior not found in type relayer.RelayMeterYAMLConfig -- this setting was REMOVED: the relayer now refuses a relay whose budget it cannot verify, and never chooses to serve one. ...
EXIT=1
```

**Action**: delete the key, or move it where the message says. The full list
of removed keys is in the v0.1.0 release notes.

A retired Redis namespace prefix. Real output:

```
Error: config is INVALID: failed to load config: invalid config: redis.namespace no longer supports per-family prefixes, and yours customizes 1 of them: cache_prefix: "custom" (keys now use "cache"). ...
EXIT=1
```

**Action**: remove the `redis.namespace.*_prefix` lines. **Stop and ask a
human** if keys really live under a custom prefix today: the fleet has to be
drained and migrated first, and the old value must not be folded into
`redis.namespace.base_prefix`.

A serving binary (not `validate`) with an unknown key warns and starts; with
`--strict-config` it refuses instead. Run `validate` to see the list.

## Redis

Both binaries check Redis at startup and exit if it is not safe to write to.
Real output:

```
Error: redis is not configured for this miner: redis maxmemory is 0 (no memory limit): set it below the memory its container has, so Redis refuses writes instead of being killed
EXIT=1
```

```
Error: redis is not configured for this relayer: redis maxmemory-policy is "allkeys-lru": set it to noeviction, or Redis silently drops the nodes a claim is proved from
EXIT=1
```

Before exiting, the relayer also logs
`Redis not operable: no new work admitted until it has room` with
`"reason":"misconfigured"`.

**Action**: load [config.redis.example.conf](../../config.redis.example.conf)
and set `maxmemory` below the memory Redis may use. Check:

```bash
redis-cli CONFIG GET maxmemory-policy
redis-cli CONFIG GET maxmemory
```

Expect `noeviction` and a number above 0. In compose, run them as
`... exec redis redis-cli ...`. The official Redis image ships no
`redis.conf`: the file must be passed to `redis-server` as its first argument,
as the compose example does.

**Redis version**: both binaries refuse to start on Redis older than 8.10;
v0.1.0 was built, tested and measured on 8.10.1. The refusal quotes what Redis
reported, for example `redis_version is 7.2.4: this release runs on Redis 8.10.0
or newer; upgrade Redis`. A Redis-compatible server that is not Redis (Valkey)
is refused by name, and so is a version string that cannot be read. Managed
offerings still on 7.x are out. Check with
`redis-cli INFO server | grep redis_version`. The check runs on every sample,
not only at startup: if Redis is replaced by an older server while the process
runs, admission closes with reason `misconfigured` until a supported one
answers. `validate` does not connect to Redis and does not check any of this.

## Miner does not start

The miner reads the chain before it consumes a single relay, and exits if it
cannot. In compose it restarts (`restart: on-failure`); under systemd,
`Restart=on-failure` does the same.

Node unreachable. Real output (node gRPC refused the connection):

```
Error: failed to start supplier worker: cannot start without the node's network: rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:1: connect: connection refused"
EXIT=1
```

**Action**: check `pocket_node.query_node_grpc_url` (`host:port`, no scheme)
and `pocket_node.grpc_insecure` (`false` for TLS endpoints, `true` only for a
plain-text node), and that the host can reach it. Each startup read waits up to
10 seconds.

Wrong network. From the source (`miner/startup_chain_state.go`):
`cannot start: the node is on another chain: the node reports network "<network>" and this miner is configured for chain "<chain_id>"`.

**Action**: set `pocket_node.chain_id` to what the node reports (`pocket` is
mainnet, `pocket-lego-testnet` the beta testnet), or point at the right node.
**Stop if** you are not sure which network the supplier is staked on.

Other chain reads, from the source: `cannot start without the chain's shared params`,
`cannot start without the chain's committed height`. **Action**: the node is
reachable but not serving queries or not synced; fix the node.

Missing block time. Real output:

```
Error: config is INVALID: failed to load config: invalid config: block_time_seconds is required and must be positive (got 0): it is the basis of every claim and proof transaction deadline, and there is no safe default -- set it to the measured block time of the network this miner runs against
EXIT=1
```

**Action**: set `block_time_seconds` to the network's measured block time
(mainnet is roughly 60).

`miner validate` checks neither the chain nor Redis: a config that validates
can still fail here.

## Relayer up but not ready

The relayer starts without the chain or the miner, but refuses every relay
until the miner has published its service factor manifest to Redis. Real
output with no miner running:

```
$ curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:8081/ready
no service factor manifest: the miner has not published one yet HTTP=503
$ curl -s -w ' HTTP=%{http_code}\n' http://127.0.0.1:8081/health
OK HTTP=200
```

and in its log:
`no service factor manifest yet -- relays are refused until the miner publishes one`.

**Action**: start the miner (or fix why it does not start), with the same
Redis in `redis.url` for both. `/ready` turns `READY` (200) by itself once the
manifest arrives; no relayer restart is needed.

`/health` is liveness only: it answers 200 whenever the process runs. Use
`/ready` for load balancer and container health checks.

The relayer also logs `RPC health check failed (continuing anyway)` and
`gRPC health check failed (continuing anyway)` when the node is unreachable at
startup; it keeps running, but cannot validate sessions until the node answers.

## Memory and CPU

Each process logs its memory limit at startup. Real output with `GOMEMLIMIT`
set:

```
{"level":"info","source":"env","fallback":"","base_bytes":1073741824,"limit_bytes":1073741824,"margin_bytes":134217728,...,"message":"process memory limit set"}
```

`"source":"cgroup"` means it came from the container or systemd memory limit.
`no memory limit found: the process runs without one` (warn) means neither was
found: the garbage collector and the miner's memory brake then work against
the host's whole RAM.

For CPU, the first line on stderr says what `GOMAXPROCS` is:
`maxprocs: Honoring GOMAXPROCS="2" as set in environment` is set;
`maxprocs: Leaving GOMAXPROCS=18: CPU quota undefined` means the process takes
every core of the host, and sizes its worker pools and Redis pool from them.

**Action**: set `GOMEMLIMIT` about 10% below the container's `mem_limit` (or
the unit's `MemoryMax`) and `GOMAXPROCS` equal to its CPU limit.

## Relays refused

| Answer | Reason label on `ha_relayer_relays_rejected_total` | Cause | Action |
|---|---|---|---|
| 503 `relayer is not admitting relays right now` | `pricing_unavailable` | no service factor manifest yet | [Relayer up but not ready](#relayer-up-but-not-ready) |
| 503 `relayer is not admitting relays right now` | `publish_queue_full` | the queue of relays waiting to be written to Redis is full | check Redis latency and health; the queue drains by itself |
| 429 `relayer is not admitting relays right now` | `validation_queue_full` | an optimistic service's validation queue is full | wait for `Retry-After`; if persistent, raise the service's capacity |
| 429 `relayer is not admitting relays: storage saturated` | `storage_saturated` | Redis has less than 1 GiB free, or 1/8 of `maxmemory` when that is smaller | raise `maxmemory`, or let the miner drain; admission reopens at twice that line (2 GiB free with a `maxmemory` of 8 GiB or more; 256 MiB free with the compose example's `1gb`) |
| relay refused before any backend call | `no_local_signer` | the relay names a supplier whose key this relayer does not hold | add the key to `keys.keys_file`, or check the gateway targets the right supplier |

Per-relay rejections log at debug level only; count them with the metric,
on the relayer's metrics port (`metrics.addr`, 9090 by default).

## Still stuck

Collect, and give a human: the output of both `validate` commands, the
`Error:` line and the last 50 log lines of the failing process, `redis-cli INFO server`
and `redis-cli CONFIG GET maxmemory*`, and the image tag or `pocket-relay-miner version`
of both processes.
