# Standalone mode: the relayer and the miner in one process, no Redis

`pocket-relay-miner standalone` runs the relayer and the miner in one process,
from one config file, with their state in an embedded store on local disk and
no Redis. It is meant for one host that serves and claims for its own
suppliers.

```bash
pocket-relay-miner standalone validate --config config.standalone.yaml
pocket-relay-miner standalone --config config.standalone.yaml
```

The relay miner runs in one of two modes:

| | standalone mode | high-availability mode |
|---|---|---|
| run | `standalone` | `relayer` and `miner`, one or more of each |
| state | an embedded store on the host's disk | one Redis shared by every relayer and miner |
| replicas, failover | no: one process | yes: miners share suppliers through leases, relayers scale out |
| to operate | one process and a directory | the processes, and a Redis 8.10+ with `maxmemory` and `noeviction` |

Standalone mode does not replace high-availability mode, and a standalone
config has no `redis` section: to run on Redis, run the two subcommands.

## What it runs

The relayer and the miner are the same code the two subcommands run, built
from the same config keys with the same defaults. What differs is the process
around them:

| | high-availability mode (`relayer` + `miner`) | standalone mode |
|---|---|---|
| processes | two | one |
| config files | two | one: common sections once, `relayer:` and `miner:` for the rest |
| supplier keys | each process loads them | loaded once, used by both sides |
| metrics and pprof | one server per process | one server for both |
| memory limit (`GOMEMLIMIT`) | one per process | one, shared: the relayer's queues and the miner's trees count against the same limit |
| logging, metrics, pprof defaults | each subcommand's own | the miner's: metrics on at `:9092`, pprof off, async JSON logging |
| shared metrics both sides write without a `component` label (the `ha_cache_*` hits, misses, chain queries and block height) | one series per process | both sides on the same series: counters add up, gauges hold the last write |
| relay queue, sessions, dedup marks, relay meter counters, caches, SMST trees, claim/proof tracking and rebroadcast messages | Redis | the embedded store (`storage.path`) |
| leader election, supplier leases | Redis, shared by the replicas | in the process: it leads and holds every supplier |

## Config

Start from [config.standalone.example.yaml](../config.standalone.example.yaml).

- At the top level, once: `pocket_node`, `keys`, `logging`, `metrics`, `pprof`,
  and `storage` (the embedded store: `path`, required, and `sync_interval`).
  There is no `redis` section: writing one is an error.
- Under `relayer:`: every other key of
  [config.relayer.example.yaml](../config.relayer.example.yaml), with the same
  meaning and default. One of its `redis` keys still applies:
  `batch_publish_interval_ms`, how often the budget charges of relays served
  and not yet mined are written; the others (pool sizes) size nothing.
- Under `miner:`: every other key of
  [config.miner.example.yaml](../config.miner.example.yaml).
  `redis.claim_idle_timeout_ms` is how long a relay handed back waits before it
  is delivered again.

A top-level section written again inside `relayer:` or `miner:` is an error. So
is `redis.url` or `redis.namespace` inside a side, and a key at the top level
that is not one of the sections above. Unknown keys are reported with their
line in your file: as a warning at startup, as an error with `--strict-config`,
and always as an error from `standalone validate`.

Flags: `--config` and `--strict-config`.

## The embedded store

Everything the process keeps lives in an embedded database under
`storage.path`: the relay queue between the relayer and the miner, the sessions
and their dedup marks, the relay meter's counters, the caches, the session
trees (SMST) the claims and proofs are built from, and the claim and proof
tracking. Keep it on local disk.

- New work stops while the disk holding `storage.path` has less than 1 GiB
  free (or an eighth of a disk smaller than 8 GiB): the relayer answers new
  relays as the HA relayer does when Redis is full, and the miner stops taking
  relays from the queue. Work resumes with 2 GiB free for the relayer and
  1.5 GiB for the miner.

- A write reaches the operating system before the call returns, so a crash or
  a kill of the process loses nothing.
- The disk is synced every `storage.sync_interval` (default 1 s), not write by
  write. An operating system crash or a power loss can lose what was written
  since the last sync: relays served in that window are not claimed, and the
  relay meter forgets the budget they used. The root a claim is built from is
  synced to disk when it is stored, and so is a write that records a claim or
  proof transaction.
- Within the embedded store a relay is counted once whatever is lost: its dedup
  mark, its session counters and the removal of its queue entry are written
  together, and the database recovers to a prefix of what was written.
- A standalone process is its own leader and holds every supplier its keys
  name; it shares them with no other process. Run one process per set of
  supplier keys: two would each serve and claim the same suppliers.

### Inspecting the store

No other process can open the store while the standalone process runs, not
even to read it, so the `redis` subcommands of high-availability mode have a
counterpart that asks the process itself. Turn on its read-only inspect server:

```yaml
inspect:
  enabled: true
  addr: "127.0.0.1:9094"   # the default; a non-loopback address is refused
```

and, on the same host (or in the same pod):

```bash
pocket-relay-miner standalone inspect sessions --supplier pokt1abc... [--state claim_tx_error] [--json]
pocket-relay-miner standalone inspect supplier --list
pocket-relay-miner standalone inspect streams [--supplier pokt1abc...]
pocket-relay-miner standalone inspect smst --session <id> [--supplier pokt1abc...]
pocket-relay-miner standalone inspect dedup --session <id>
pocket-relay-miner standalone inspect meter [--session <id>]
pocket-relay-miner standalone inspect submissions [--supplier pokt1abc...] [--failed-only]
```

`sessions`, `supplier` and `submissions` print what their `redis` counterparts
print, `--json` included. The server answers GET only, on loopback only, and
reads without changing anything: a session past its TTL is left out of the
answer, not deleted. It has no authentication, which is why it never listens
on another address. `leader`, `pubsub`, `keys` and `flush` have no counterpart:
there is no election, the bus is in-process, and nothing here writes.

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
  `relayer` subcommand. The observability server's `/ready` answers once the
  miner side is up.
