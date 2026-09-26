# Contributing to Pocket RelayMiner

These rules apply to everyone who changes this repository, human or AI. To
deploy the relay miner instead, start at [AGENTS.md](AGENTS.md).

## What you are working on

Production software that handles real value. The relayer targets 1000+ RPS per
replica, so every millisecond on the hot path counts; the miner turns served
relays into claims and proofs that get the supplier paid.

- **Language**: Go, the version in `go.mod` (CI builds with the same).
- **State**: all session state is in Redis. No component keeps it on local disk.
- **Two processes, one binary**: the relayer is a stateless multi-transport
  proxy that validates relays, signs responses and publishes them to Redis
  Streams, routing to backends by the `Rpc-Type` header (1=gRPC, 2=WebSocket,
  3=JSON_RPC, 4=REST, 5=CometBFT). The miner consumes those streams, builds SMST
  trees in Redis and submits claims and proofs. Relayers receive block events
  only through Redis pub/sub, published by the miner.

| Package | What it holds |
|---|---|
| `main.go`, `cmd/` | the CLI: `relayer`, `miner`, `redis` (debug subcommands in `cmd/redis/`), `relay` (the load-test client in `cmd/relay/`), `version` |
| `relayer/` | the relayer: proxy, relay validation, metering, signing, WebSocket bridge, health checks |
| `miner/` | the miner: stream consumption, SMST in Redis, session lifecycle, claims and proofs, supplier management |
| `cache/` | L1 local (`xsync`), L2 Redis and L3 chain caches, with pub/sub invalidation |
| `rings/` | ring signature verification and application delegation rings |
| `keys/` | supplier key providers (keys file, keyring) and their hot reload |
| `leader/` | the miner's global leader election over a Redis lock |
| `tx/` | the transaction client: claims and proofs broadcast, permits, inclusion reads |
| `query/` | on-chain query clients |
| `client/` | the block subscriber the miner requires, and `relay_client/`, which builds and signs relays for the CLI |
| `transport/` | the mined-relay types and codec; `redis/` holds streams, the publisher, the consumer, store health and the KeyBuilder (`namespace.go`); `grpcconn/` builds every gRPC connection to a full node |
| `pool/` | backend endpoint pools with selection and a circuit breaker |
| `config/` | configuration shared by both binaries, and the retired-key table |
| `observability/` | the metrics and pprof server and shared registries |
| `logging/` | structured logging and goroutine panic recovery |
| `internal/` | `memlimit` (memory limit at startup), `testredis` (the real Redis for tests), `conventions` (tests that enforce these rules) |
| `proto/` | protobuf definitions |
| `docs/`, `examples/`, `scripts/`, `tilt/` | operator docs, runnable examples, test and ops scripts, the Tilt dev environment |

Design with the gateways that send relays in mind: how they pick suppliers,
retry, hold WebSocket sessions and read our errors decides what a change to the
relayer means for the traffic it serves. Public docs describe only this
repository's behaviour and name no other product; say "the gateway/client that
sends relays".

## Development environment

Development runs on [Tilt](https://tilt.dev/) over a local kind cluster: a
localnet chain, Redis, the relayer and the miner, test backends, Prometheus and
Grafana. Tilt watches the tree, rebuilds and restarts pods on every change, and
proxies every port.

```bash
make tilt-up-k8s     # start (requires a kind cluster)
make tilt-down-k8s   # stop
```

With Tilt up, the relayer is at `localhost:8180` (the target of every direct CLI
and load test), Prometheus at `localhost:9091`, Grafana at `localhost:3000`.
Setup: [tilt/README.md](tilt/README.md), [docs/testing/TILT.md](docs/testing/TILT.md);
direct CLI tests of every transport: [docs/testing/DIRECT_CLI.md](docs/testing/DIRECT_CLI.md).

While Tilt runs: never `kubectl port-forward`, never build by hand, never delete
pods. Logs: `kubectl logs -l app=<service>`.

```bash
make build          # development build: ./bin/pocket-relay-miner
make build-release  # optimized, statically linked
make install-hooks  # once: pre-commit runs the static gate, pre-push runs level 2
```

Build binaries with `make`, never `go build` (the Makefile sets flags and the
version). Run tests with `make` too, except when debugging one package.

## Workflow

1. Branch from `main`.
2. Commit with [Conventional Commits](https://www.conventionalcommits.org/):
   `<type>(<scope>): <subject>`, types `feat`, `fix`, `perf`, `refactor`,
   `docs`, `test`, `build`, `ci`, `chore`. A title plus at most one paragraph;
   a message that needs more is a commit doing more than one thing.
3. No AI attribution in commits or PR bodies: no `Co-Authored-By` for an AI, no
   "Generated with" footer.
4. Open a PR to `main`. The title uses the commit format; the description is
   bullets: what changed, how it was tested, what breaks.
5. PRs are squash-merged.
6. A `vX.Y.Z` tag on `main` publishes `ghcr.io/pokt-network/pocket-relay-miner:vX.Y.Z`
   plus `latest`, and opens a draft release.

Everything tracked is in English: code, comments, docs, skills, scripts, commit
messages. The static gate fails on Spanish in any tracked file.

## Code standards

**Errors**
- Always check them; wrap with `fmt.Errorf("context: %w", err)`.
- Never `panic()` in production code.
- Startup errors propagate through error channels, not `os.Exit`; never
  `logger.Fatal` in a goroutine.

**Logging** (structured: `logger.Info().Str("key", value).Msg("message")`)
- Anything that fires once per relay, message or connection -- rejections,
  meter denials, backend failures included -- logs at `Debug` only. Its
  alertable signal is a metric with a bounded `reason` label.
- State changes (failover, config reload, circuit breaker, rebalance,
  reconnect) log at `Info` or `Warn`: they fire once per change.
- A per-message condition that reveals a producer defect (a malformed stream
  message) may stay at `Warn`.
- `Error` only for what needs immediate attention.
- Never log private keys or credentials.

**Concurrency**
- Bounded concurrency with `github.com/alitto/pond/v2`. Never an unbounded
  `go func()`: use a pool, or wrap a long-lived goroutine in
  `go logging.RecoverGoRoutine(logger, "name", fn)(ctx)`.
- `xsync.Map` (puzpuzpuz/xsync/v4) for concurrent maps, never `sync.Map`.
- `context.Context` for cancellation; always defer `Close()` or cleanup.
- `Stop()` / `Close()` / `Shutdown()` are idempotent (`sync.Once` for channel
  closes). Close a replaced connection before overwriting a pool entry.
- A package var a test overrides needs a happens-before edge between the test's
  write and every read. Reading it on the constructor's goroutine is not that
  edge: the caller can be another test's server goroutine. When a test's
  server outlives `srv.Close()` (a hijacked WebSocket handler does), the test
  waits for its own handler to return.

`internal/conventions` enforces the pool, `xsync`, panic, sleep, metric-label,
miniredis and Redis-key rules, and freezes the pre-existing exceptions.

**Metrics**
- No high-cardinality labels: no URLs, no session IDs.
- Delete an unused metric at once.
- Record metrics asynchronously on hot paths (the MetricRecorder pattern).

**Performance**
- Profile before optimizing (`go test -cpuprofile=cpu.prof -bench .`, then
  `go tool pprof cpu.prof`) and prove the change with benchmark numbers.
- Redis pipelining for batches, pre-allocated slices, no allocations on hot paths.
- Targets per replica: relay validation and signing < 1 ms, SMST update
  < 100 µs, cache L1 hit < 100 ns, L2 < 2 ms, L3 miss < 100 ms. These are
  targets: quote a measured figure only from a run you cite.
- HTTP pool sizing: connections needed = RPS × backend latency. The defaults
  are in `DefaultConfig()` in `relayer/config.go`.

**Failure behaviour**
- Redis unavailable: the relayer fails closed on admission; a relay whose budget
  the meter cannot verify is refused (`ErrMeterStoreUnavailable`). A chain query
  blinking is different: that relay is served and the miner arbitrates.
- Chain unreachable: the miner retries with exponential backoff.

**Security**: validate all external input, compare secrets in constant time,
sanitize errors returned to clients.

```go
func ProcessRelay(ctx context.Context, relay *Relay) error {
    if err := relay.Validate(); err != nil {
        // Per-request rejection: Debug plus a metric, never Warn.
        relaysRejected.WithLabelValues(relay.ServiceID, rejectReasonValidationFailed).Inc()
        logger.Debug().Err(err).Str("session_id", relay.SessionID).Msg("relay validation failed")
        return fmt.Errorf("validation failed: %w", err)
    }
    result, err := processWithTimeout(ctx, relay)
    if err != nil {
        return fmt.Errorf("processing failed: %w", err)
    }
    logger.Debug().Str("session_id", relay.SessionID).
        Int64("compute_units", result.ComputeUnits).Msg("relay processed")
    return nil
}
```

## Redis

### Keys and channels: the KeyBuilder STRONG RULE

Every Redis key and pub/sub channel is built through the KeyBuilder
(`transport/redis/namespace.go`, reached with `client.KB()`). No exceptions.

- Never `fmt.Sprintf("ha:...")` or any hardcoded prefix, in code, the CLI or
  script docs.
- One KeyBuilder method per key pattern and per channel. Publisher and
  subscriber call the same method; a channel with no method gets one.
- Only `base_prefix` is configurable. Every segment below it is a constant, so
  a new family is a new method, never a new config knob.
- The golden table in `transport/redis/namespace_test.go` pins every method's
  output and is the authoritative key list. Changing a constant is a breaking
  change across versions. `TestKeyBuilder_NoTwoMethodsCollideUnderAnyNamespace`
  and `TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily` guard collisions; they
  pass uniform arguments, so they cannot see a collision that needs specific ones.

### Inspecting Redis

Use this product's `pocket-relay-miner redis` subcommands first: they build keys
through the KeyBuilder and decode what raw Redis cannot. `redis-cli` is the
fallback.

```bash
pocket-relay-miner redis leader
pocket-relay-miner redis sessions --supplier pokt1abc... --state active
pocket-relay-miner redis smst --session <id>
pocket-relay-miner redis streams --supplier pokt1abc...
pocket-relay-miner redis cache --type application --list
pocket-relay-miner redis keys --pattern "ha:*" --stats
pocket-relay-miner redis pubsub --channel "ha:events:cache:application:invalidate"
pocket-relay-miner redis dedup --session <id>
pocket-relay-miner redis supplier --list
pocket-relay-miner redis meter --session <id>      # or --all
pocket-relay-miner redis submissions --supplier pokt1abc... [--failed-only]
pocket-relay-miner redis flush --pattern "ha:test:*"   # destructive, asks first
```

## Tests

Every change passes `make fmt lint test` before it is done. A feature that spans
components is tested at three levels:

1. **Unit**: happy paths with production-shaped data; error paths with equal
   weight (malformed, empty, nil, not found, refused, canceled, timeout, gRPC
   codes); edge cases (zero, negative, boundaries, overflow); every field of a
   returned struct checked; errors checked with `errors.Is` against the
   sentinel; no branching on the test's name.
2. **Integration**: wired as production wires it, testing the pipeline and the
   state transitions (active → claiming → claimed → proved).
3. **Live**: on Tilt, after any change to startup wiring, config parsing or
   relay routing, and for anything touching relay, claim, proof, settlement or
   metering.

**Rule #1 (cannot be broken):**
- Every test passes `go test -race` with no warnings.
- Every test is deterministic: no `time.Sleep()` for synchronization, no
  dependency on an ordering that is not guaranteed.
- A test that fails once in 1000 runs is fixed or deleted. "Pre-existing" is
  not an excuse.

**Redis in tests is a real Redis.** `internal/testredis` gives each test a
client and a key prefix on the Redis 8.10.1 that `scripts/gates/redis.sh up`
starts on `127.0.0.1:6399`. miniredis is not used: it answers a blocking
`XREADGROUP` at once, never ages the pending list and approximates expiry.
`internal/conventions` freezes the files still on it and fails on a new one.

Also: use real implementations rather than mocks; put test-only code behind
`-tags test`; give every store or shared state a concurrent read+write test
under `-race`; test every optional component as nil; check that each assertion
proves what you meant (`len(result) != 0` is not `result[0].Address == want`).
Benchmarks for SMST and cache: `go test -bench=. -benchmem ./miner/` and
`./cache/`.

## Quality gates

The gates are scripts in `scripts/gates/`, so a human, CI and an agent run the
same implementation. They report and never fix.

| level | command | covers |
|---|---|---|
| 1 | `make gate LEVEL=1` | gofmt, build, vet, golangci-lint, tracked files, Spanish, unreachable functions -- both Go modules |
| 2 | `make gate LEVEL=2` (the default of `make gate`) | level 1 plus the test suite (including `internal/conventions`), the race detector and the coverage run |
| 3 | `make gate LEVEL=3` | level 2 plus live validation on Tilt, claim and proof verified on chain |

Narrow a level to one package with `PKG=miner make gate`.

- `make fmt` rewrites files; the gate only reports them.
- The coverage run is what CI rejects on, and instrumentation widens timing: a
  green suite with a red coverage run is a real red.
- A skipped gate prints NOT RUN. That is not green.
- A red gate is diagnosed, not re-run until green: a red that goes away on a
  re-run is a schedule the test does not control.

## What gets committed

The only documentation this repository tracks is written for the people who run
the software: `README.md`, `AGENTS.md`, `CONTRIBUTING.md`, `CLAUDE.md`, `docs/`
(a feature's usage doc looks like `docs/SIMULATED_RELAYS.md`), the `scripts/`
READMEs, and `.claude/skills/` (reviewed as code: English, no personal paths, no
operator data).

Never tracked; keep on disk under `scripts/localonly/` (gitignored):

- plans, specs, brainstorms, design notes, hand-overs, review reports;
- operator infrastructure data: hostnames, IPs, ssh aliases, supplier addresses,
  internal URLs, topology, keys. A tracked script that needs such data reads it
  from `scripts/localonly/<area>/`, with a placeholder `.example` beside the
  script (`scripts/loadtest/backends.sh` and `backends.conf.example` are the
  model). Before editing a tracked file, grep it for operator strings;
- editor configuration and the rest of `.claude/`.

A `.go` file under `scripts/localonly/` compiles into `go test ./...`; keep saved
code under a `_`-prefixed directory, which the toolchain ignores.

`.gitignore` does not untrack a file already tracked. `make check-tracked-files`
(run by CI) fails on a tracked-and-ignored file or a working-document path;
untrack with `git rm -r --cached <path>`, never `git add -f`.

Code comments state constraints the code cannot show, and never point at a
design doc.

## Common tasks

**A new cache type**: the interface in `cache/interface.go`; the L2 layer with
pub/sub and KeyBuilder keys; wiring in `cache/orchestrator.go`; the leader's
refresh; metrics in `cache/metrics.go`; tests on `internal/testredis`.

**Per-service backend pool sizing**: `scripts/loadtest/backends.sh` measures each
backend's ceiling and the concurrency that keeps p99 under a budget; see
[scripts/loadtest/README.md](scripts/loadtest/README.md).

## License

Contributions are licensed under the project's license (see [LICENSE](LICENSE)).
