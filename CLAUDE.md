# CLAUDE.md

This file tells Claude Code (and any human reading along) how to work in this
repository. It has two audiences; read the section that is yours.

## If you are here to deploy or operate

You do not need the rest of this file. Start here:

- [AGENTS.md](AGENTS.md) -- the entry point for an agent deploying or operating
  the relayer and the miner.
- [docs/deploy/README.md](docs/deploy/README.md) -- the deployment guide.
- [examples/docker-compose](examples/docker-compose) -- a runnable example.

The minimum you must know:

- **Kubernetes is not a supported deployment target in v0.1.0.**
- **Tilt (`Tiltfile`, `tilt/`) is the DEVELOPMENT environment**, a local kind
  cluster that rebuilds on every file change. It is not a deployment path and
  its configs are not production configs.

## If you develop this repository

Everything below is for the people who change the code (today: Jorge and Otto).

### Contributing guidelines

**All contributors (human and AI) must follow [CONTRIBUTING.md](CONTRIBUTING.md).**

- **Conventional Commits**: `feat:`, `fix:`, `perf:`, etc. for automated changelogs.
- **Workflow**: dev -> main -> vX.Y.Z release pipeline.
- **Code standards and testing**: see CONTRIBUTING.md; all tests pass before
  committing (`make fmt lint test`).
- **Commit authorship**: do NOT add `Co-Authored-By: Claude ...` or any AI
  attribution footer to commit messages. The repository does not attribute AI
  work in git history -- commits are authored as the developer running the session.

### Core principles

**YOU ARE NOT A FRIEND. YOU ARE A PROFESSIONAL SOFTWARE ENGINEER.** This code
handles real money and must scale to 1000+ RPS per replica. Clean, structured,
tested code is mandatory, and every millisecond on the hot path counts.

- **Verify everything, provide evidence.** Never state an assumption as fact.
  Support assertions with file paths and line numbers, links or command output.
- **Be critical, not complacent.** Never say "looks good" without evidence (test
  or build output). Challenge claims from the user or another agent by verifying
  them independently. A review that finds nothing is suspicious. If you disagree
  with an approach, say so with reasoning; do not silently comply with a bad plan.
- **Demand clarity.** If a task is ambiguous, ASK before implementing. If you lack
  the motivation for a change, ask for it. If requirements conflict with existing
  code patterns, flag the conflict explicitly.
- **Evidence-based reporting.** A bug report shows the file, line and why it is
  wrong. A fix report shows before/after and the test that proves it. "No issues
  found" explains what was checked and how.
- **When you don't know, SAY SO.** Do not guess. Search the code, read the tests,
  check imports, ask. Then answer with the location, e.g. "invalidation is
  triggered via Redis pub/sub, see `cache/<file>.go:<lines>`".
- **Design with the gateways that send relays in mind.** Their behaviour
  constrains ours: how they pick suppliers, retry, hold WebSocket sessions and
  read our errors decides what a change to the relayer means for the traffic it
  serves. Check a change against that client side before calling it done. The
  public docs name no other product, so a rule here says "the gateway/client
  that sends relays".

### Before the first edit -- THE COUNCIL, then the success criterion

**Invoke the council before choosing the approach to ANY fix or feature, and
invoke the `andrej-karpathy-skills:karpathy-guidelines` skill before the first
edit.** Both, in that order, and neither conditional on the task looking small.

The council reads the SPACE OF APPROACHES before you pick one. A code review
afterwards does NOT fill its slot: a review reads code you already wrote, so the
approach it critiques is the one you already committed to. If no external
provider is configured, run the local council (`claude-council:local-council-execution`)
and say in the report that its members share a model, so their agreement is a
common prior to stress-test, not corroboration.

**The council's own output goes to a FILE under `scripts/localonly/<item>/`, not
only into the conversation.** Measured 2026-09-20: a three-lens council on SMST
compression ran as subagents, its findings were distilled into the queue item, and the
transcript carrying its reasoning and its rejected alternatives was gone after the next
compact -- so the session that came to implement it had to ask where the council was, and
the honest answer was "it ran, and only its conclusions survive". A distilled conclusion
cannot be re-examined; the argument behind it can. Write the synthesis down when it comes
back, with the tensions and what each lens rejected.

**A red on a fix is not an exemption either.** When a test or a live run goes red
on a fix, the next step is a council on the WHOLE design with every red so far,
not a patch for the last red -- and whoever supervises cannot waive the council for
the one writing. Measured 2026-09-17 (item 321): "the test found the defect and
the fix follows the existing pattern" waived it twice in a row, and the result was
patch on patch -- live-heap brake, overage, hysteresis, unload with TryLock,
deadlock, in-place re-import -- each chosen against the last red only.

Measured 2026-08-30, and it is why this paragraph lives here: the council rule
lived only in `.claude/skills/item/SKILL.md`, and `CLAUDE.md` did not mention the
council at all. That session invoked karpathy -- because THIS file demands it
unconditionally -- edited `relayer/config.go`, committed, and only then ran the
council. The council immediately found that the fix touched one of THREE
forwarding paths (`relayer/proxy.go` and `relayer/healthcheck.go` never normalize
a backend URL), so the commit turned "invalid config that fails on the first
relay" into "config certified valid by `relayer validate` that fails on the first
relay" -- worse than not fixing it. The commit was reverted. A `feedback_*` memory
saying exactly this was auto-loaded in that session's context and did not fire
either: **a rule only executes from the place that is read before anyone chooses
what to invoke.**

Karpathy's rule 4 is the load-bearing one here: **write the success criterion down
before starting, as something checkable.** "It works" is not one. "Level 2 passes
and the new test goes red when the sentinel is removed from the classifier" is
one. If the criterion cannot be written down, the task is not understood yet --
say so instead of starting.

This lives in CLAUDE.md and NOT in a skill on purpose: a rule that has to fire
before anyone chooses to invoke something cannot itself depend on being invoked.
Measured 2026-08-26 -- `test-teeth` already said "passed with the defect present
-- the test is decoration", `TestIsPermanentKeyFailure` was exactly its case, and
nobody pointed the skill at it across four commits of the same defect
(`efd2fb8` -> `fb46b85` -> `47116f8` -> `2ce1d30`, the last reintroducing the
failure mode the previous one had just fixed).

### Closing a session

**Use the `close-session` skill.** The work does not end at the hand-over -- it
ends at asking for push and PR. `scripts/localonly/QUEUE.md` holds ONLY the items the
maintainer approved. EVERY finding is put to the maintainer with two questions -- add it
to the queue or not, open an issue or not -- and waits in the hand-over's "Proposed
findings" section until answered. Nothing is queued or filed on your own, and the
canonical hand-over is whatever `scripts/handoff-index.sh` says it is, never the
newest by date.

### Architecture at a glance

- **Language**: Go 1.26.5 (see `go.mod`; CI builds with the same).
- **Shape**: distributed services with Redis-backed state. **ALL session state is
  in Redis -- no local disk storage.**
- **Transports**: JSON-RPC (HTTP), WebSocket, gRPC, REST/Streaming (SSE).
- **Targets**: 1000+ RPS per relayer replica; 99.9% uptime with automatic failover.

1. **Relayer** (`relayer/`): stateless multi-transport proxy. Validates relay
   requests (ring signatures, sessions), signs responses with supplier keys,
   publishes to Redis Streams, routes to backends by the `Rpc-Type` header
   (1=gRPC, 2=WebSocket, 3=JSON_RPC, 4=REST, 5=CometBFT). Relayers receive block
   events ONLY through Redis pub/sub published by the miner (no CometBFT WebSocket
   per relayer, no config option), so every relayer sees the same blocks as the
   miner; RPC/gRPC endpoints are used for startup health checks (`/status` and
   gRPC health, non-blocking).
2. **Miner** (`miner/`): stateful claim/proof submission with leader election.
   Consumes Redis Streams, builds SMST trees in Redis, submits claims and proofs.
3. **Cache** (`cache/`): three tiers with pub/sub invalidation -- L1 local
   (`xsync` map), L2 Redis (shared), L3 chain queries. The distributed-lock retry
   when several instances repopulate at once sleeps 5 ms (it was 100 ms). The
   case behind it: on a cache invalidation every relayer tried to repopulate L1
   at once, and the 100 ms sleep made ~43 of 793 load-test requests slow; at
   5 ms the load test's p50 went from 73 ms to 22 ms (figures as recorded when
   the change was made, not re-run since).
4. **Rings** (`rings/`): ring signature verification and application delegation
   rings (copied from poktroll). Target: <5 ms per verification.

Critical files:

- Entry points: `main.go`, `cmd/cmd_relayer.go`, `cmd/cmd_miner.go`,
  `cmd/cmd_redis.go` (debug subcommands in `cmd/redis/`), `cmd/cmd_relay.go`
  (the relay/load-test client, implementation in `cmd/relay/`).
- Core logic: `relayer/proxy.go`, `relayer/relay_processor.go`,
  `relayer/relay_meter.go`, `miner/smst_manager.go`, `miner/lifecycle_callback.go`
  and `miner/supplier_manager.go` (claim/proof submission), `cache/orchestrator.go`.
- Storage: `miner/redis_mapstore.go` (Redis-backed SMST, implements
  `kvstore.MapStore`), `transport/redis/publisher.go`, `transport/redis/consumer.go`,
  `transport/redis/namespace.go` (every key and channel).
- Tests worth knowing: `miner/redis_mapstore_test.go`, `miner/redis_smst_bench_test.go`.

Dependencies (exact versions in `go.mod`): poktroll (protocol types, query
clients, crypto), go-redis v9 (state, streams, pub/sub, locks), Cosmos SDK
(tx building, signing, keyring).

### Development environment and commands

**READ THIS BEFORE SUGGESTING COMMANDS.**

1. **Building**: `make build` (development) and `make build-release` (production).
   NEVER `go build` directly to produce a binary -- the Makefile handles flags,
   versioning and cross-compilation. (The gate scripts compile with `go build ./...`
   as a check; that is not a build.)
2. **Tilt**: ALL services run in the local kind cluster via Tilt (`tilt up` /
   `tilt down`). Tilt watches files, rebuilds and restarts pods, and proxies every
   port. So: NEVER `kubectl port-forward`, NEVER a manual build while Tilt runs,
   NEVER delete pods by hand. Check the Tilt UI for URLs. The relayer is reached
   directly at `localhost:8180`, which is the target of every end-to-end and load
   test -- relays are not measured through a gateway. Logs:
   `kubectl logs -l app=<service>`; exec: `kubectl exec -it <pod> -- <command>`.
3. **Testing**: `make test`, `make test-coverage`, `make test_miner` (miner with
   race detection), the scripts in `scripts/` and the guides in `docs/testing/`
   (e.g. `./scripts/test-chaos.sh`, or
   `pocket-relay-miner relay jsonrpc --localnet --service develop-http`). The
   localnet services are `develop-http`, `develop-websocket`, `develop-grpc` and
   `develop-stream`.
   NEVER run tests without `make` unless debugging one package.
4. **Code quality**: `make fmt`, `make lint`, `make tidy`.
5. **Benchmarks**: `go test -bench=. -benchmem ./miner/` (SMST), `./cache/`.
6. **Redis**: inspect it with this product's own `pocket-relay-miner redis ...`
   subcommands first (see "Debugging Redis"); `redis-cli` (proxied by Tilt) is the
   fallback for what they do not show.

Working order for a change: read the code first (grep, don't assume), check
what imports the package you touch (`go list -f '{{.ImportPath}}' -deps ./... |
grep <package>`), run the existing tests first so you have a baseline, write the
tests first (TDD), implement with error context and logging at key points,
benchmark critical paths, then run the quality gates.

### Code standards

**Error handling**
- ALWAYS check errors; wrap with `fmt.Errorf("context: %w", err)`.
- Log errors with context at the level the logging policy assigns to the code path --
  a per-request error is `Debug` plus a metric.
- Never `panic()` in production code paths.

**Logging**
- Structured: `logger.Info().Str("key", value).Msg("message")`, with the context
  fields needed for debugging. Never log private keys or credentials.
- Per-request logs (including relay REJECTIONS, meter denials, backend
  failures): `Debug` level only -- never Info/Warn/Error on a path that
  fires once per relay/message/connection. The alertable signal for a
  per-request condition is a METRIC with a bounded `reason` label
  (`relays_rejected_total`, `relays_dropped_total`, ...), not a log line:
  under flood (Redis outage, stake exhausted, broken gateway) a per-relay
  Warn is one line per relay per instance.
- State changes (failover, config reload, circuit breaker transition,
  rebalance, reconnect): `Info` or `Warn` -- these fire once per change,
  and they are what an operator reads during an incident.
- A per-message condition that signals a DEFECT in a producer (malformed
  stream message, empty RelayHash) may stay at `Warn`: it is bounded by
  the defect existing, and it must be visible without debug logging.
- `Error` only for things that need immediate attention.
- Never `logger.Fatal` in goroutines -- use error channel propagation.

**Concurrency**
- USE worker pools (`github.com/alitto/pond/v2`) for bounded concurrency -- the
  library this repo actually uses; the previously documented `sourcegraph/conc`
  has zero imports here.
- NEVER an unbounded `go func()` -- use a pond pool, or wrap a genuinely
  long-lived goroutine in `go logging.RecoverGoRoutine(logger, "name", fn)(ctx)`
  so a panic is counted and logged instead of crashing the process.
  `internal/conventions` freezes the existing bare `go` statements and fails on
  new ones.
- Use `xsync.Map` (puzpuzpuz/xsync/v4) for lock-free concurrent maps -- never
  `sync.Map` (enforced by `internal/conventions`).
- Protect shared state with `sync.RWMutex` when necessary; use `context.Context`
  for cancellation and timeouts; ALWAYS defer `Close()` or cleanup.
- Pattern: `pool := pond.NewPool(10); pool.Submit(func() { ... }); pool.StopAndWait()`.
- **A package var a test overrides needs a happens-before edge between the
  test's write and every read of it -- and "read it on the caller's goroutine"
  is NOT that edge.** This rule used to say "capture it into a struct field at
  construction, because the constructor runs on the caller's goroutine". That
  fix was applied (`relayer/websocket.go` captures `wsFirstFrameWait` into
  `firstFrameWait` at construction) and the race SURVIVED it: measured 2026-09-20,
  `-race -count=5 ./relayer/` went red in 9 of 10 invocations. The reason is
  precise and it is the whole lesson: **the rule assumed the constructor's caller
  is the test.** Here the caller was an `httptest.Server` handler belonging to
  ANOTHER test, and the trace showed that goroutine as `(finished)` -- so the two
  never even ran at the same time. Nothing was racing in parallel; there was
  simply no edge, because `httptest` calls `wg.Done()` at `StateHijacked` (the
  instant of the upgrade) and `srv.Close()` therefore returns without waiting for
  the handler body. **What fixed it: the test WAITS for its own handler to
  return** (a `WaitGroup` the handler marks on exit, with a bounded wait that
  `t.Error`s by name rather than hanging the package). That does not prevent
  anything -- it creates the edge. Two corollaries paid for the same day:
  `wsMaxMessageBytes` is NOT a safe contrast -- it has a second read in
  `ensureBackend` reached from `Run`'s goroutine, so construction-capture cannot
  order it even in principle; and a comment asserting safety because "no test
  calls `t.Parallel()`, so tests never overlap" is true in its premise and false
  in its conclusion -- what outlives a test is a goroutine of its server, not an
  overlapping test. Such a comment is worse than none: it actively discourages
  looking. The class is enforced, not remembered: `internal/conventions` fails on
  a test assigning a package var declared outside tests, with the 43
  pre-existing ones frozen (AST, and it counts `.Store()` too, or making the var atomic would
  satisfy the guard without fixing anything).

**Metrics**
- No high-cardinality labels (no URLs, no full session IDs as Prometheus labels).
- Delete unused metrics immediately -- no dead declarations.
- Record metrics asynchronously on hot paths (the MetricRecorder pattern; the
  relay meter latency histogram `relay_meter_latency_seconds` in
  `relayer/metrics.go` is recorded this way, in both the eager and the optimistic
  validation modes).

**Cleanup/shutdown**
- `Stop()` / `Close()` / `Shutdown()` must be idempotent (`sync.Once` for channel closes).
- Always `Close()` replaced connections before overwriting pool entries.
- Startup errors propagate via error channels, not `os.Exit`.

**Performance**
- Profile before optimizing: `go test -cpuprofile=cpu.prof -bench .`, then
  `go tool pprof cpu.prof`; identify the hot path; reduce allocations, use
  `sync.Pool`, batch operations; verify with concrete benchmark numbers; comment
  the non-obvious optimization.
- Use Redis pipelining for batch operations; pre-allocate slices when the size is
  known; avoid allocations in hot paths.
- Targets per replica: relayer 1000+ RPS sustained; relay validation and signing
  <1ms average; SMST update <100µs; cache L1 hit <100ns, L2 hit <2ms, L3 miss
  <100ms. These are TARGETS; before quoting any measured figure, re-run the
  benchmark or the load test and cite that run.
- HTTP pool sizing: required connections = RPS x backend latency (1000 RPS at
  500 ms needs 500; the old limit of 100 bottlenecked at 100 ms of backend
  latency). The defaults are 500/100/500 (`MaxIdleConns` /
  `MaxIdleConnsPerHost` / `MaxConnsPerHost`, `DefaultConfig()` in
  `relayer/config.go`, documented in `config.relayer.example.yaml` and validated
  by `config.relayer.schema.yaml`).
- The load-test client (`cmd/relay/http.go`) verifies the supplier signature and
  the JSON-RPC error field and relay protocol compliance of every response, so
  it counts only valid relays;
  that costs throughput and is deliberate.

**Security**
- Never log private keys or credentials; validate all external input (relay
  requests, API calls); constant-time comparison for sensitive data; rate
  limiting against DoS; sanitize error messages exposed to clients.

**Failure behaviour**
- **Redis unavailable**: the relayer FAILS CLOSED on admission -- there is no
  setting for this any more. The knob that existed (`relay_meter.fail_behavior`)
  was erased on 2026-08-31 and left a tombstone that warns; a relay whose budget
  the meter cannot verify is refused, because the store holds what the session
  already consumed. A CHAIN query blinking is the other half and is NOT the same:
  that one is served and the miner arbitrates. See `relayer/relay_meter.go`,
  `ErrMeterStoreUnavailable`.
- Blockchain unreachable: the miner retries with exponential backoff.
- Leader failure: standby takes over within 5 seconds.
- High latency: circuit breaker prevents cascading failures.

### Test quality requirements (NON-NEGOTIABLE)

Tests exist at THREE levels, all required for a feature spanning components.

**Level 1 -- unit tests.** Every test file covers:
1. **Happy paths**: every public function's primary use with realistic,
   production-shaped data.
2. **Error/wrong paths, equal priority**: invalid input (malformed, empty, nil),
   missing data (not found, empty responses, null fields), network failures
   (refused, canceled, timeout), gRPC codes (NotFound, Unavailable, DeadlineExceeded).
3. **Edge cases**: zero, negative, empty strings and collections, boundaries
   (max int64, overflow), concurrent access under the race detector.
4. **Field-level verification**: a struct with 5 fields gets 5 checks, not "returned something".
5. **Error type verification**: `errors.Is()` against the sentinel, not "an error occurred".
6. **No magic strings in test logic**: no `if tt.name == "special case"`; use struct fields.

**Level 2 -- integration tests (per feature flow).** Wiring matches production
(if it diverges, the test is worthless); test the pipeline, not the parts ("relay
validated but publish fails" is integration, "ValidateRelay rejects bad signature"
is unit, both are needed); test state transitions end to end (active -> claiming
-> claimed -> proved).

**Level 3 -- live validation (Tilt/localnet)** with real network calls, configs
and Kubernetes, via the scripts in `scripts/`; run after any change touching
startup wiring, config parsing or relay routing.

**Cross-cutting**
- **Rule #1 (CANNOT BE BROKEN)**: no flaky tests, no race conditions, no
  exceptions. Everything passes `go test -race`; tests are deterministic (no
  `time.Sleep()` for synchronization, no ordering dependencies); a test that
  fails once in 1000 runs is fixed or deleted; "pre-existing" is not an excuse.
- Use `-tags test` for test-only code. Use real implementations, not mocks. For
  Redis that means a REAL Redis: `internal/testredis` (Redis 8 on 127.0.0.1:6399,
  started by `scripts/gates/redis.sh up`). miniredis is eradicated since
  2026-08-19 -- it answers a blocking XREADGROUP immediately, never ages the PEL,
  and approximates expiry, and a consumer that could not shut down reached
  production behind a green suite. `internal/conventions/miniredis_fake_test.go`
  freezes the four files still on the fake and fails on any new one.
- Every store or shared state has a concurrent read+write test under `-race`.
- Every optional component is tested as nil; absent features must not panic.
- Self-review every assertion: does it prove what you think? `len(result) != 0`
  where `result[0].Address == expected` was meant passes for the wrong reason.

Example of the expected shape:

```go
func ProcessRelay(ctx context.Context, relay *Relay) error {
    logger := logging.ForComponent(logger, "relay_processor")
    if err := relay.Validate(); err != nil {
        // Per-request rejection: Debug + metric, never Warn (see Logging).
        relaysRejected.WithLabelValues(relay.ServiceID, rejectReasonValidationFailed).Inc()
        logger.Debug().Err(err).Str("session_id", relay.SessionID).Msg("relay validation failed")
        return fmt.Errorf("validation failed: %w", err)
    }
    result, err := processWithTimeout(ctx, relay)
    if err != nil {
        return fmt.Errorf("processing failed: %w", err)
    }
    logger.Debug().Str("session_id", relay.SessionID).
        Int64("compute_units", result.ComputeUnits).Msg("relay processed successfully")
    return nil
}
// BAD: func ProcessRelay(relay *Relay) { relay.Validate(); process(relay) }
```

### Quality gates (mandatory for every change)

The gates are scripted under `scripts/gates/` (`make gate LEVEL=...`; the `gates`
skill says which level). Every change passes ALL of these before it is done:

1. **Build**: `go build ./...` -- zero errors
2. **Tests**: `go test -tags test ./...` -- all pass
3. **Race detector**: `go test -tags test -race ./...` -- zero races
4. **Vet**: `go vet ./...` -- no issues
5. **Lint**: `make lint` -- no issues
6. **Format**: `gofmt -l .` -- no files listed
7. **Self-review** of the diff: unused imports/variables, missing error handling,
   shared state without sync, log levels (no Info/Warn on hot paths), metric
   cardinality, DRY violations.

If any gate fails, fix it before reporting completion. **No "pre-existing"
excuses**: either your change broke it or it was already broken -- either way,
diagnose and fix it.

### Measuring, and when a result is evidence -- earned 2026-09-07

Every rule here has its measured case attached, deliberately: a rule with a case
fires when it is needed, an enunciated one does not. Measured the same day, twice
in opposite directions -- the rule about a var a test overrides fired on its own
while writing the var, because it carries the `wsFirstFrameWait` case; the rule
about the test Redis, written as a bare fact, bit its own author twenty minutes
after he wrote it.

**AN EMPTY RESULT IS NOT EVIDENCE UNTIL A CONTROL SAYS THE TOOL LOOKED.** Ask the
same tool, at the same moment, something whose answer you already know. Without a
control, "nothing found" and "asked wrong" are the same signal. Four ways it
happened in one day, all silent:

- A probe under a directory whose name starts with `_` -- the Go toolchain ignores
  it, so the linter reported nothing and it read as "the linter does not catch
  this".
- A `grep` for a phrase in a wrapped document: the line break splits the phrase,
  the grep is empty, and someone nearly EDITED A CORRECT TEXT to match the broken
  measurement. That direction is the worst: it introduces a defect through the
  act of verifying.
- A count taken with a narrower filter than the question. `max-issues-per-linter`
  defaults to 50 and `max-same-issues` to 3, so a lint total of exactly 50 is a
  cap, not a count -- the real number was 294. **A total equal to a well-known
  default is a claim about tooling.** And the fix is not "widen the filter":
  widening turned counting imports into counting mentions, which is a different
  question. Ask what your filter counts. Same shape, 2026-09-20, and worth naming
  because the filter looked like the obvious one: `grep -v '^#'` over
  `scripts/gates/deadcode-allowlist.txt` to "get the real entries" dropped the
  paragraph that answered the question -- in that file most reasons sit at the end
  of their line, but the load-bearing one sits ABOVE its entry. The conclusion
  drawn from the filtered view ("this line is obsolete, delete it") was the exact
  inverse of what the comment said, and acting on it would have armed a future
  gate failure. **An allowlist keeps its data in the lines and its reasons in the
  comments, so stripping the comments is a partial read dressed as an inventory.**
  Before proposing that a line be deleted from one, read that line's comment.
- **A file with NUL bytes: `grep` prints NOTHING, not even the `0` of no matches.**
  A retention file had 1448 contiguous NULs from an interrupted write, and every
  search in it came back silent. `file` said `data`, not text. This one is worse
  than the others because the question was right and the TOOL chose to go quiet.

**A COUNT IN A COMMIT MESSAGE IS COUNTED IN THE TREE, ALL OF THEM.** Twice in one
day a message said "four" where there were five, both times a total that was true
before the last edit: a member was added, the sentence describing it was updated,
and the old total stayed in front of it. The tell is that **the sentence adding
the new member is the one still saying the old number**. When corrected on one
number, verify the others -- the second time, that is how the wrapped-grep trap
above surfaced.

**AN INSTRUMENT BUILT FROM THE DEFECT'S OWN MATERIAL CANNOT DETECT IT, AND IT FAILS
GREEN.** Measured 2026-09-20, closing item 388 -- a wall-clock instant crossed as an
`int64`, which loses the monotonic reading `Sub` needs. A council had already caught
that the liveness test's fake clock started from `time.Unix(1_700_000_000, 0)`, a
`Time` with no monotonic reading, so a correct implementation and a broken one passed
it identically; the fix was to start from `time.Now()`. That fix was applied, and it
was **not enough**: one level below, the test's `fakeClock` stored its instant in an
`atomic.Int64` via `UnixNano()` and rebuilt it with `time.Unix(0, ...)` -- **the exact
round trip the item is about**. The instrument destroyed the property the test existed
to assert, so the test still could not discriminate, and the written criterion claimed
it now did. What found it was the new positive control going red against a mark stamped
by a REAL dispatch round, not by a test's `Store`. So: when the defect is a PATTERN
(a lost property, a dropped field, a truncated value), grep the test harness for that
same pattern before trusting any green it produces -- the harness is code, written by
the same hands, and a partial fix one layer up reads exactly like a complete one.
**The same trap wears a second costume: synthetic test DATA that does not behave like
the real thing.** Measured the same day, adding node compression: two controls that
assert a large write is split into chunks built their nodes with
`bytes.Repeat([]byte("v"), 1024)`. Real nodes are mostly hashes and do not compress;
that filler compresses to tens of bytes, so 600 nodes started fitting in ONE `HSET`
and both controls went green while controlling nothing. They only went red because
they assert the split itself. Filler is a stand-in for the real value's SHAPE, not just
its size -- here, chained SHA-256.

**A GATE WRITES TO A FILE, AND THE FILE CARRIES ITS OWN `EXIT=$?`.** Never a
`tail` with a fixed count: one ate the NAME of the failing check, which sat in
the MIDDLE of the output, not at the end. And the harness's own completion notice
reports the exit code of the LAST command in the pipeline, not the gate's -- a
gate that exited 2 was announced as `exit code 0`. Writing `EXIT=$?` inside the
log makes the artefact self-sufficient instead of depending on the reader
remembering to distrust the notice.

**A GATE MEASURES THE TREE THAT EXISTED WHEN IT STARTED, NOT THE ONE THAT EXISTS
WHEN YOU READ ITS LOG. WHILE IT RUNS, THE TREE IS FROZEN.** `go test` compiles at
the start of every invocation, so editing between invocations of a repeat loop
silently splits it: measured 2026-09-20, a 5x`-count=5` run was aborted because a
dead field was deleted after invocation 2 -- runs 1-2 and 3-5 would have measured
different binaries and been reported as one figure. Reviewing the diff while a
gate runs is fine; touching it is not. When a repeat loop is the evidence, record
the `sha256` of the files under test alongside its log, so a later reader can
confirm every invocation saw the same tree. And an aborted run's partial logs get
DELETED, not kept: a log nobody labelled as void is a log someone will quote.

**WHEN YOU REMOVE A DEFAULT FROM A CONSTRUCTOR, ENUMERATE ITS CALLERS.** Not "does
the startup path still work" -- that is a narrower question than the change.
Measured the same day: dropping a `markSuccess()` that marked a publisher healthy
before asking Redis anything was verified against the production startup, where
hundreds of lines of wiring hide the sub-second window before the first
heartbeat, and it was correct. The test bench has no wiring and relays in
milliseconds, so the same change produced **33 failures and a 10-minute
timeout**. The question that was missing is mechanical, not intuitive: `grep -rn
"NewBatchingPublisher"` returns three sites, and the one that broke is the second
line of the test helper. This is the same move the repo already demands when you
change how a datum is PRODUCED -- enumerate the consumers and walk each one --
applied to construction. A verification can be correct and incomplete, and the
tell is that it answered about one caller. **The same holds for a change that only
moves WHEN something happens.** Measured the same day: making the publisher's first
heartbeat fire immediately instead of one tick later added no new behaviour, and broke
two tests by two unrelated paths -- it raced a test that injects its own mark, and it
opened a connection while a fixture was recording, so go-redis's per-connection init
pipeline (`redis.go:839`, which goes through the client's hooks) arrived as an extra
empty EXEC. Neither is visible in the diff, because the test harness observes TRAFFIC,
not design. After a timing change, run the packages whose tests watch traffic or
counters, with `-count=5`.

**TILT IS THE WATCHER AND THE PROXY, SO IT IS A TURN TO SHARE, NOT A NUISANCE.**
With Tilt up the live gate can reach the relayer and nobody may edit `.go`
(a rebuild competes for the machine and killed a gate by OOM); with Tilt down you
may edit and the live gate fails preflight because the port-forwards are gone --
and `kubectl port-forward` is not the way out, it is forbidden here for this
reason. Killing the watcher does not resolve the conflict, it swaps one
impossibility for the other. Whoever holds the turn says so, and the other writes
docs, reads code or drafts a commit message meanwhile.

**BETWEEN TWO SESSIONS THE TREE IS THE ARTEFACT AND THE MESSAGE IS THE REPORT.**
Both sessions once waited on each other: one had announced "I am re-running the
gate" and never sent the second message, the other had the diff on disk and was
waiting for permission that did not exist. If the work is in the tree it is ready
to read. And when finishing something, the message to the peer goes IMMEDIATELY
after verifying it, before reporting to anyone else.

### Redis

#### Key/channel construction (STRONG RULE -- no exceptions)

**Every Redis key and pub/sub channel MUST be built through the
`KeyBuilder` (`transport/redis/namespace.go`, reached via `client.KB()`).**

- NEVER `fmt.Sprintf("ha:...")` or any hardcoded prefix -- not in
  production code, not in the CLI, not in scripts' documentation of keys.
- NEVER build a channel as `somePrefix + ":suffix"` with a prefix wired
  per-component. This already caused real bugs: two shared-params caches
  listening on different channels, and a cleanup publish with zero
  subscribers, because `PubSubPrefix` was wired to `"ha:events"` in one
  binary and `EventsCachePrefix()` in another.
- One KeyBuilder method per key pattern and per channel. Publisher and
  subscriber MUST call the SAME method -- if a channel has no KB method,
  add one; do not inline the string.
- **Only `base_prefix` is configurable.** Every segment below it is a
  constant in `transport/redis/namespace.go`, so a partial namespace cannot
  produce an empty segment (`prod::application:x`) and there is nothing to
  default per-field. Do NOT reintroduce a per-family knob: one that can be
  turned until it equals another family's literal is how a key ended up with
  two writers, and how the supplier SCAN pattern could be made to match every
  cache key. New family, new KeyBuilder method -- not new config.
- Tests: golden-string tests pin each KB method's default output
  (changing a constant is a breaking cross-version change -- mixed fleets stop
  hearing each other), and a pattern test asserts every SCAN pattern matches
  only its own family. **The collision test EXISTS** -- measured 2026-09-01:
  `TestKeyBuilder_NoTwoMethodsCollideUnderAnyNamespace`
  (`transport/redis/namespace_test.go`) walks the KeyBuilder by reflection,
  and `TestKeyBuilder_PatternsMatchOnlyTheirOwnFamily` is its glob half.
  This file said the opposite until that day, because the test landed inside the
  stack and nobody came back to the sentence. What it CANNOT see, by
  construction, is a collision that only appears with specific arguments: it
  passes uniform ones. The case that motivated it -- `SupplierStateKey` and
  `SupplierRegistryKey` colliding under `supplier_prefix: suppliers`, two writers
  and mutually unparseable readers.

#### Key patterns (default `base_prefix` = `ha`)

The authoritative list is the golden table in `transport/redis/namespace_test.go`.

- **WAL**: `ha:relays:{supplierAddress}` (Redis Streams)
- **SMST nodes**: `ha:smst:{supplierAddress}:{sessionID}:nodes` (Redis Hashes)
- **Session metadata**: `ha:miner:sessions:{supplier}:{sessionID}` (JSON);
  indexes `ha:miner:sessions:{supplier}:index` and
  `ha:miner:sessions:{supplier}:state:{state}` (Sets of session IDs)
- **Deduplication**: `ha:miner:dedup:session:{sessionID}` (Set of relay hashes)
- **Leader lock**: `ha:miner:global_leader` (instance ID, TTL 30s)
- **Cache**: `ha:cache:application:{address}`, `ha:cache:service:{serviceID}`,
  `ha:cache:shared_params`, `ha:cache:proof_params` (proto bytes); locks
  `ha:cache:lock:{type}:{id}`; tracking `ha:cache:known:{type}s` (Set)
- **Meter** (per (session, supplier) -- one session is served by many suppliers
  and each meters its own stake; ephemeral, cleaned at session end):
  `ha:meter:{sessionID}:{supplier}:meta` (SessionMeterMeta JSON) and
  `ha:meter:{sessionID}:{supplier}:consumed` (consumed uPOKT counter)
- **Supplier state and fleet index** (plural is the set, singular is the entity):
  `ha:supplier:{address}` (SupplierState JSON -- the replica of the supplier's
  on-chain state; the relayer reads it to decide whether to serve) and
  `ha:suppliers:index` (Set of addresses THIS FLEET handles; read by the balance
  monitor and orphan-stream detection). There is NO `ha:suppliers:{address}`: it
  existed, had zero readers, and collided with the key above. Do not reintroduce
  a per-supplier key under the plural prefix.
- **Pub/Sub**: `ha:events:cache:{type}:invalidate` (cache invalidation),
  `ha:events:cache:invalidate:supplier_params` (supplier module params; a
  nonstandard, frozen name, `SupplierParamsInvalidateChannel`),
  `ha:events:blocks` (block events), `ha:meter:cleanup` (meter cleanup)
- **Submission tracking**: `ha:tx:track:{supplier}:{sessionEndHeight}:{sessionID}`
  (JSON: tx hashes, success/failure, error reasons, timing, relays, compute
  units). TTL 24h by default, `SubmissionTrackingTTL` / `submission_tracking_ttl`
  in `miner/config.go` (lowered from 7 days).

#### Performance characteristics

The HSET/HGET/HDEL/HLEN figures once quoted here (~28-30 µs/op) came from
miniredis, which measures a Go map, not Redis. The benchmarks in
`miner/redis_smst_bench_test.go` run against a real Redis (`internal/testredis`)
and give larger numbers that mean what an operator's Redis costs; re-run them
before quoting a figure.

#### Debugging Redis

Use this product's own `pocket-relay-miner redis` subcommands (`cmd/redis/`)
first: they build every key through the KeyBuilder and decode what raw Redis
cannot. `redis-cli` (proxied by Tilt) is the fallback for what they do not
cover (`redis-cli INFO memory`, `redis-cli MONITOR` -- very verbose,
`redis-cli HGETALL <key>`):

```bash
pocket-relay-miner redis leader                                  # leader status and TTL
pocket-relay-miner redis sessions --supplier pokt1abc... --state active
pocket-relay-miner redis smst --session session_123              # SMST node data
pocket-relay-miner redis streams --supplier pokt1abc...          # WAL + consumer groups
pocket-relay-miner redis cache --type application --list
pocket-relay-miner redis cache --type application --key pokt1abc --invalidate
pocket-relay-miner redis keys --pattern "ha:*" --stats           # keys with type/TTL stats
pocket-relay-miner redis pubsub --channel "ha:events:cache:application:invalidate"
pocket-relay-miner redis dedup --session session_123
pocket-relay-miner redis supplier --list
# meter: scans every supplier that metered the session; app stake lives inside
# the meter meta, service compute units in the service cache
pocket-relay-miner redis meter --session session_123
pocket-relay-miner redis meter --all
# claim/proof submission tracking (24h history by default)
pocket-relay-miner redis submissions --supplier pokt1abc... [--failed-only]
pocket-relay-miner redis submissions --supplier pokt1abc... --session <id> --session-end <height>
# DANGEROUS - requires confirmation
pocket-relay-miner redis flush --pattern "ha:test:*"
```

### Common tasks

**Adding a new cache type**
1. Define the cache interface in `cache/interface.go`.
2. Implement the L2 (Redis) layer with pub/sub, keys via the KeyBuilder.
3. Wire it into `CacheOrchestrator` in `cache/orchestrator.go`.
4. Add refresh logic for the leader.
5. Add metrics in `cache/metrics.go`.
6. Write tests against a real Redis (`internal/testredis`), never miniredis.

**Backend RPS ceiling loadtest (per-service pool tuning)**

`scripts/loadtest/backends.sh` measures how much each upstream RPC backend can
sustain, finds the optimal concurrency under a p99 latency budget, and produces a
`service -> max_conns` table to drive per-service pool tuning in the relayer
config. It reads operator-specific data (URLs, ssh host) from a gitignored conf:

```bash
mkdir -p scripts/localonly/loadtest
cp scripts/loadtest/backends.conf.example scripts/localonly/loadtest/backends.conf
$EDITOR scripts/localonly/loadtest/backends.conf
scripts/loadtest/backends.sh probe            # verify all backends respond
DEFAULT_REQS=20000 MAX_P99_MS=100 \
  scripts/loadtest/backends.sh sweep-optimal \
  > /tmp/optimal.csv 2> /tmp/optimal.log      # max_conns per service under p99 <= 100 ms
tail -25 /tmp/optimal.log
```

Full reference in `scripts/loadtest/README.md`. Key points:
- **Ceiling = max RPS** (no budget), what the backend delivers if latency does not
  matter. **Use `sweep`.**
- **Optimal = max RPS bounded by p99**, what to actually configure, because the
  gateway/client that sends relays penalises tail latency. **Use `sweep-optimal`.**
- **Per-replica tuning.** The script measures one client; production uses one
  pool per relayer replica, so the number is exactly the per-replica
  `max_conns_per_host`. Scaling replicas multiplies the ceiling, it does not
  divide the per-replica setting.

### Repository hygiene -- NEVER commit

#### Operator infrastructure data

Operator-specific infrastructure data (backend hostnames, IPs, ssh
host aliases, supplier addresses, on-prem topology, internal node
URLs, deploy details) **must never appear in tracked files**. This
includes scripts, configs, READMEs, examples, and code comments.

The `scripts/localonly/` directory is gitignored and is the **only**
place this data is allowed to live in the repo. When you write a
script that needs operator data:

1. Put the script itself somewhere tracked (e.g. `scripts/<area>/`)
2. Make it read its operator-specific config from a file under
   `scripts/localonly/<area>/`, with an env var override
3. Ship a `.example` file next to the tracked script that shows the
   format using **placeholder** values (`my-host`, `node.internal`)
4. Document the setup in the script's README without naming a real
   operator

The model is `scripts/loadtest/backends.sh` with `backends.conf.example`
(see "Common tasks").

Before creating or editing any tracked file, grep for known
operator strings (hostnames you've seen in the conversation, ssh
aliases, etc.) to make sure none leaked in. If you discover leakage
in already-tracked files, fix it immediately and tell the user.

#### Planning documents

**The only documentation this repository tracks is documentation written
for the people who run this software.** Everything you write to organize
your own work -- plans, specs, brainstorms, design notes, phase summaries,
handoffs, review reports, task state -- is a working artifact. It goes
stale the moment the work it describes ships, and it is noise to every
reader who was not in the session that produced it.

- NEVER `git add` a plan, spec, brainstorm, handoff, or phase summary.
  Not under `.planning/`, not under `docs/`, not "just this one for
  context". Tooling that wants to write `.planning/` or `.gsd/` is
  writing scratch -- let it, but never track it.
- Working documents live in `scripts/localonly/` (gitignored) or an
  ignored directory. They stay on disk; they just never reach a commit.
- The committed deliverable for a feature is its **usage doc**: what
  it does and how to run it, in `docs/`, written for an operator who
  has never read your plan. `docs/SIMULATED_RELAYS.md` is the model.
- Public docs describe only this repository's behaviour -- no other products.
- Code comments state constraints the code cannot show. A comment must
  never point at a design doc -- the doc will move or die, and the reader
  needs the invariant, not its provenance.

**`.gitignore` is not the guard** -- it does nothing for a file that is
already tracked, which is exactly how `.planning/` and `.idea/` lived in
this repository for months. The guard is `make check-tracked-files`
(CI runs it on every PR): it fails on anything tracked-and-ignored, and
on known working-doc paths. If it fires, untrack with
`git rm -r --cached <path>` -- never `git add -f` past it.

### What this is

Production software handling real value, which must scale horizontally, stay
maintainable and debuggable, perform under load and fail safely. It is not a
place for banter, unverified assumptions, "good enough" code or unverified
performance claims. **Your job is to maintain these standards rigorously.**
