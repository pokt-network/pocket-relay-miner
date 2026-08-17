# Quality gates

The checks this repository runs before it believes a change is good.

They live here as plain shell scripts so that the same implementation serves
three callers — a person at a terminal, CI, and an agent. When a gate is green
for you it means what it means in CI, because it is the same code.

```bash
make gate                # level 2 (default): everything that runs without a cluster
make gate LEVEL=1        # static only, seconds
make gate LEVEL=3        # + live validation on Tilt
PKG=miner make gate      # narrow to one package
scripts/gates/race.sh    # or call one gate directly
```

## The levels

| level | gates | cost | what it proves |
|---|---|---|---|
| 1 | `static` | seconds | the tree compiles, is formatted, passes vet and lint, and tracks nothing local-only |
| 2 | `+ tests`, `race`, `coverage` | minutes | the suite passes, no data races, and it survives coverage instrumentation |
| 3 | `+ live` | tens of minutes | relays are actually mined, claimed, proved and settled on-chain |

Levels are cost tiers, not importance tiers. Level 1 says nothing about
behaviour, and level 2 says nothing about whether a relay earns money — only
level 3 exercises the path that does.

## The contract every gate keeps

- **Exit 0 passed, non-zero failed. The last line is the verdict**, so a caller
  keeping only the tail still learns the outcome.
- **A gate reports; it never fixes.** A gate that rewrites files hides the
  failure it just found, and in a pre-commit context it rewrites the tree after
  git has already snapshotted the index. `make fmt` is the fixer; the gate tells
  you to run it.
- **No side effects**: gates do not touch git, the index, or the working tree.
  The one exception is `coverage.sh`, which writes `coverage.out` — that profile
  is the point of the run, and it is gitignored.
- **A missing tool is a SKIP, never a PASS**, counted and named in the verdict.
  "I found nothing" and "I did not look" must not produce the same signal.
- **`PKG=<pkg>` narrows** any gate to one package.

## The gates

| script | what it runs |
|---|---|
| `lib.sh` | shared output helpers and the verdict. Sourced, not executed. |
| `static.sh` | gofmt · go build · go vet (twice: plain and `-tags test`) · golangci-lint · tracked-file guard, across **both** Go modules (root and `tilt/backend-server`). `--staged` judges formatting on staged files only — that is how the pre-commit hook calls it. |
| `tests.sh` | `go test -tags test`. The `test` tag is not optional: test-only helpers live behind it. |
| `race.sh` | `go test -race -count=1`. `-count=1` defeats the result cache, which would otherwise satisfy the command with a PASS from a run without `-race`. |
| `coverage.sh` | the coverage profile — what CI rejects on. |
| `live.sh` | the money path on the Tilt localnet: load through the relay CLI at `:8180`, then claim and proof inclusion asserted **on-chain**. `--preflight-only` checks readiness and stops. |
| `all.sh` | runs the above up to a level. Fail-fast; `--keep-going` for the full picture. |

`live.sh` **never starts or stops anything.** If the localnet is not up it prints
the `tilt up` command and exits non-zero, because bringing the cluster up claims
ports and containers another session on the same machine may be using. Run
`--preflight-only` first: it reads state and touches nothing.

**It asserts the FINAL settlement outcome, not inclusion.** A claim can land
on-chain and still expire without its proof, be discarded, or get its supplier
slashed — every one of those happens *after* inclusion, so "the claim is
on-chain" says nothing about whether the relays were paid. The gate reads the
terminal events the chain emits in its EndBlocker, from `block_results`:

| event | meaning |
|---|---|
| `EventClaimSettled`, status `1` | PROVEN — the relays were paid |
| `EventClaimSettled`, status `3`, or `EventClaimExpired` | expired: served, claimed, never paid |
| `EventSupplierSlashed` | staked funds burned |
| `EventClaimDiscarded` | dropped without settling |

Passing requires at least one claim settled as proven, zero expiries, zero
slashing and zero discards. Nothing settling at all is a failure, not a pass:
the run proved nothing about payment.

These come from the validator directly and **not** from the miner's settlement
metrics, because `settlement_monitor.enabled` defaults to false — the block is
commented out in `config.miner.example.yaml`, which is the file Tilt renders its
config from, so on a stock localnet those series are empty and asserting on them
would be asserting on nothing.

`cache` and `miner` run sequentially when targeted on their own: their tests
share a single miniredis fixture, so parallelism races the fixture instead of
testing the code.

## Gotchas paid for

- **A data race is not a flake.** The detector reports a race it observed; a
  green on the next run means the goroutine schedule differed. Re-running until
  green is how a race reaches production.
- **Passing `tests` and failing `coverage` is a real result**, not a
  contradiction: instrumentation widens timing and surfaces flakes a plain run
  hides. Coverage is what CI runs, so it is the answer that counts.
- **A stray `.go` file under `scripts/localonly/` joins the build.** git ignores
  the directory; the Go toolchain walks the filesystem and does not. Keep saved
  code under a `_`-prefixed directory — Go ignores `_` and `.` prefixes.
- **`go build` and a plain `go vet` do not compile test code.** Test-only helpers
  live behind `//go:build test`, so deleting a symbol that only tests use passes
  both and fails minutes later in the test gate. That is why `static.sh` vets
  twice, the second time with `-tags test`.

## Adding a gate

Source `lib.sh`, call `gate_repo_root`, report through `gate_step` /
`gate_pass` / `gate_fail` / `gate_skip`, and end with `gate_verdict <name>`.
Then add it to the level list in `all.sh`.

Before you trust it, **prove it can fail**: inject the defect it claims to catch
and confirm it goes red, then revert and confirm green with a clean `git diff`.
A gate that cannot go red is decoration.
