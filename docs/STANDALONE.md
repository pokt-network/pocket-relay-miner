# Standalone: the relayer and the miner in one process

`pocket-relay-miner standalone` runs the relayer and the miner in one process,
from one config file. It is meant for one host that serves and claims for its
own suppliers.

```bash
pocket-relay-miner standalone validate --config config.standalone.yaml
pocket-relay-miner standalone --config config.standalone.yaml
```

Standalone does not replace running `relayer` and `miner` as separate
processes. That remains the deployment for more than one relayer or miner.

## What it runs

The relayer and the miner are the same code the two subcommands run, built
from the same config keys with the same defaults. What differs is the process
around them:

| | `relayer` + `miner` | `standalone` |
|---|---|---|
| processes | two | one |
| config files | two | one: common sections once, `relayer:` and `miner:` for the rest |
| supplier keys | each process loads them | loaded once, used by both sides |
| metrics and pprof | one server per process | one server for both |
| memory limit (`GOMEMLIMIT`) | one per process | one, shared: the relayer's queues and the miner's trees count against the same limit |
| logging, metrics, pprof defaults | each subcommand's own | the miner's: metrics on at `:9092`, pprof off, async JSON logging |
| shared metrics both sides write without a `component` label (the `ha_cache_*` hits, misses, chain queries and block height) | one series per process | both sides on the same series: counters add up, gauges hold the last write |
| Redis | required | required in this version |

## Config

Start from [config.standalone.example.yaml](../config.standalone.example.yaml).

- At the top level, once: `pocket_node`, `keys`, `logging`, `metrics`, `pprof`,
  and `redis` with `url` and `namespace` (and nothing else: a `redis` key that
  sizes a client goes under the side whose client it sizes).
- Under `relayer:`: every other key of
  [config.relayer.example.yaml](../config.relayer.example.yaml), with the same
  meaning and default. That includes its own `redis` keys
  (`batch_publish_interval_ms`, `pool_size`, ...).
- Under `miner:`: every other key of
  [config.miner.example.yaml](../config.miner.example.yaml), with its own
  `redis` keys (`claim_idle_timeout_ms`, `pool_size`, ...).

A top-level section written again inside `relayer:` or `miner:` is an error. So
is `redis.url` or `redis.namespace` inside a side, and a key at the top level
that is not one of the sections above. Unknown keys are reported with their
line in your file: as a warning at startup, as an error with `--strict-config`,
and always as an error from `standalone validate`.

Flags: `--config`, `--strict-config`, and `--redis-url`, which sets the URL for
both sides.

## Startup and shutdown

- The miner starts first and the relayer once the miner is serving. The
  relayer's `/ready` answers 503 until the miner has published its service
  factors, as with the two subcommands.
- If either side fails to start, the other one is stopped and the process
  exits non-zero. The same happens when the miner fails while serving (its
  leader controller) or when either side panics.
- On SIGINT or SIGTERM the relayer stops first and the miner after it. Each
  has the same 30 s drain window as its subcommand (the relayer's proxy drain,
  the miner's supplier worker close); the closes that follow those windows are
  not bounded by them. Give the process a stop timeout of at least 60 s
  (`TimeoutStopSec`, or the container's grace period), more if you measure
  longer. A signal while a side is still starting cancels that startup.
- The relayer's health server (`relayer.health_check`) works as it does in the
  `relayer` subcommand. The observability server's `/ready` checks the miner's
  Redis connection.
