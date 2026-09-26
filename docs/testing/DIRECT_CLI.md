# Testing Relays Directly via the CLI

This is the **direct** way to test the relayer: the built-in `relay` command
sends signed relay requests **straight to a relayer replica** (`:8180` on the
Tilt localnet), with no gateway in between. Use it when you need honest
per-relay results — signature verification, error codes, and per-protocol
behavior.

## Why direct

A client that judges a relay by its HTTP status can count failures as
successes: a gateway may answer its own client with `200 OK` and an empty body
when the relayer behind it returned a `503`. A load run measured that way can
report every request as `OK` while the WAL stays empty (`XLEN 0`) — not one
relay mined. That makes such a measurement wrong for **every** question,
throughput and lifecycle included, not just for error paths.

The `relay` CLI talks to the relayer directly and validates the full response
(supplier signature + the backend's own error field), so a failure is a
failure. This is the tool for correctness testing per protocol and for load.

## Prerequisites

- A running localnet (see [TILT.md](TILT.md)).
- The binary: `make build` produces `./bin/pocket-relay-miner`. Examples below
  use `pocket-relay-miner`; substitute `./bin/pocket-relay-miner` if it is not
  installed in a directory your shell searches.

`relay` takes the mode as an argument (`relay jsonrpc`, `relay grpc`, ...);
every mode accepts the same flags, listed by `pocket-relay-miner relay --help`.

## Transports and RPC types

The relayer routes each relay to a backend by its **RPC type** — the Pocket
protocol's `RPCType`, carried in the `Rpc-Type` header (or, for gRPC, request
metadata). The protocol defines five, and the relayer supports all of them:

| `RPCType` | Value | Backend type | Wire shape |
|---|---|---|---|
| `GRPC` | 1 | `grpc` | protobuf over HTTP/2 cleartext (h2c) |
| `WEBSOCKET` | 2 | `websocket` | JSON over WebSocket |
| `JSON_RPC` | 3 | `jsonrpc` | JSON-RPC over HTTP |
| `REST` | 4 | `rest` | HTTP (incl. SSE streaming) |
| `COMET_BFT` | 5 | `cometbft` | CometBFT RPC — JSON-RPC over HTTP/WS |

`JSON_RPC`, `REST` and `COMET_BFT` are all JSON-over-HTTP at the wire; they
differ only in the routing hint and the backend they map to. `GRPC` (protobuf)
and `WEBSOCKET` are the structurally distinct ones.

The CLI has one mode per transport it drives today, each sending a fixed
`Rpc-Type`:

| CLI mode | `Rpc-Type` sent |
|---|---|
| `jsonrpc` | 3 (JSON_RPC) |
| `websocket` | 2 (WEBSOCKET) |
| `grpc` | 1 (GRPC) |
| `stream` | 4 (REST) |
| `cometbft` | 5 (COMET_BFT) |

The `cometbft` mode sends a CometBFT JSON-RPC request (default `status`); a
CometBFT backend is just a JSON-RPC-over-HTTP endpoint (e.g. a CometBFT node's
`:26657` RPC) that the relayer routes to on `Rpc-Type: 5`.

Which backend(s) a service exposes is **per-deployment configuration**, not a
fixed property of the relayer. The Tilt localnet configures these services:

| Localnet service | Backend type | Backend URL |
|---|---|---|
| `develop-http` | `jsonrpc` | `http://backend:8545` and `http://backend-2:8545` (round-robin) |
| `develop-websocket` | `websocket` | `ws://backend:8545/ws` |
| `develop-grpc` | `grpc` | `backend:50051` |
| `develop-stream` | `rest` | `http://backend:8545/stream/sse` |
| `develop-cometbft` | `cometbft` | `http://validator:26657` |

Each service also has a twin with the other validation mode
(`develop-http-eager`, `develop-websocket-optimistic`,
`develop-grpc-optimistic`, `develop-stream-optimistic`,
`develop-cometbft-optimistic`), same backends; `--localnet` knows their app
keys too.

On your own deployment a single service can expose several backend types at
once; the relayer picks one per relay by the `Rpc-Type` header. An HTTP relay
that carries no `Rpc-Type` goes to the service's `default_backend` (`jsonrpc`
when that is unset).

## `--localnet`: zero-config defaults

`--localnet` fills in the local Tilt environment: the relayer URL
(`http://localhost:8180`), the chain gRPC endpoint (`localhost:9090`), the chain
ID, the default supplier (the first localnet supplier), the localnet gateway
key, and it **auto-selects the app key** for the `--service` you name (each
localnet service is staked to its own app; an unknown service falls back to the
`develop-http` app). Any of these you pass yourself wins. So the minimal
invocation is just a mode + `--service`:

```bash
# One JSON-RPC relay, full diagnostic output (timings, signature, payload)
pocket-relay-miner relay jsonrpc --localnet --service develop-http
```

A successful diagnostic prints `Status: ✅ SUCCESS`, `Signature: ✅ VALID`,
`Error Check: ✅ NO ERRORS`, and the response payload (pretty-printed when it is
JSON, raw otherwise).

## Single relay per protocol (smoke test)

```bash
pocket-relay-miner relay jsonrpc   --localnet --service develop-http
pocket-relay-miner relay websocket --localnet --service develop-websocket
pocket-relay-miner relay grpc      --localnet --service develop-grpc
pocket-relay-miner relay stream    --localnet --service develop-stream --batches 3
pocket-relay-miner relay cometbft  --localnet --service develop-cometbft
```

Notes per protocol:

- **grpc** — by default the CLI sends a *real* unary gRPC request
  (`demo.DemoService/GetBlockHeight`) so it exercises the relayer's native
  gRPC (h2c) forwarding end to end, and prints the decoded `Block Height`.
  That method only exists on the localnet demo backend, so **against any other
  backend the default returns `grpc-status 12` (UNIMPLEMENTED)** — use
  `--grpc-method` to name a method the backend actually serves:

  ```bash
  # a Cosmos SDK full node: GetLatestBlock takes a request with no fields
  pocket-relay-miner relay grpc --service <svc> --keys-file keys.yaml \
    --node <host:port> --chain-id <id> \
    --relayer-url http://<relayer>:8180 --supplier <addr> \
    --grpc-method /cosmos.base.tendermint.v1beta1.Service/GetLatestBlock

  # a request WITH fields: hex-encoded protobuf, here demo.BlockRequest{number:42}
  pocket-relay-miner relay grpc --localnet --service develop-grpc \
    --grpc-method /demo.DemoService/GetBlock --grpc-request-hex 082a
  ```

  `--grpc-request-hex` defaults to empty, which is the correct body for any
  request message with no fields, so most methods need only `--grpc-method`.
  A non-zero `grpc-status` prints the backend's own `grpc-message`, which is
  usually the line that says which method or field was wrong. Neither flag can
  be combined with `--payload`: a custom payload drives the relayer's REST
  fallback, which never builds a gRPC request.
- **stream** — the CLI reads the SSE stream until the **server closes it** (EOF)
  or the client `--timeout` (default 120s) fires, collecting *every* signed batch
  the service sends. This mirrors a real network, where the client cannot know
  how many batches a service will emit — it just drains until close.
  `--batches N` asks the demo backend (localnet only) to emit exactly `N`
  batches and then close, so `--batches 5` returns 5 batches instead of
  streaming forever. Without `--batches`, the localnet demo feed is infinite, so
  the run ends on `--timeout` and returns whatever it collected (batches already
  received are never discarded). `-n`/`--count` does **not** apply to stream
  (use `--batches`), and `--load-test` is not supported. Each batch is
  signature-verified individually. `--output-json` prints the combined stream
  payload raw instead of pretty-printed; the other modes ignore it.
- **cometbft** — sends a CometBFT JSON-RPC request (default `status`) tagged
  `Rpc-Type: 5`; the localnet `develop-cometbft` service routes it to the
  validator's CometBFT RPC (`validator:26657`), so the response is the real node
  `status` (network, moniker, latest block height). Use `--payload` to call a
  different method (e.g. `health`, `abci_info`). Diagnostic only — load testing
  goes through `jsonrpc`.

## Custom payloads (`--payload`)

`--payload` replaces the request body the mode would send (by default an
`eth_blockNumber` JSON-RPC call for `jsonrpc`). Pass it inline, or as `@FILE` to
read it from a file:

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --payload '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'

pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --payload @request.json
```

A body over about 128 KiB can **only** be passed as `@FILE`: the kernel caps a
single command-line argument at that size. A missing, unreadable or empty file
is an error (the CLI never falls back to the default body). A file over 10 MiB
— the relayer's stock `default_max_body_size_bytes` — still goes out, with a
warning on stderr that the relayer may refuse it for size unless the service
raises `max_body_size_bytes`.

## Load testing (`--load-test`)

Add `--load-test` with `-n` (total requests) and `--concurrency` (workers).
Optionally `--rps N` to cap the rate. Not supported for `stream` or `cometbft`.

```bash
# 1000 JSON-RPC relays, 50 workers
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --load-test -n 1000 --concurrency 50
```

The summary reports total/successful/errors, success rate, throughput, and
p50/p95/p99 latency. A relay counts as **successful only** if the supplier
signature verifies **and** the decoded response carries no error — a signed
backend error (e.g. HTTP 500/415) is correctly counted as a failure with the
reason shown in the error breakdown.

A WebSocket load test prints two more lines: `Lost to session rollover: N` and
`WebSocket pool: size=... redials=...`. Every relay it asked for ends as
successful, an error, or lost: a relay the relayer answered by ending the
connection's session (the session rolled over) is counted as lost, not as an
error, and the connection is redialed.

### `--all-suppliers`: do not pin one supplier

A single supplier exhausts *its* per-session claimable budget quickly while the
other session suppliers sit idle. `--all-suppliers` says "any of them, do not
make me name one":

- in a **load test** it round-robins across every supplier in the current
  session, matching how a gateway distributes traffic;
- for a **single relay** it picks one of the session's suppliers at random.

```bash
# 60 relays fanned out across all session suppliers, per protocol
pocket-relay-miner relay jsonrpc   --localnet --service develop-http       --load-test -n 60 --concurrency 5 --all-suppliers
pocket-relay-miner relay websocket --localnet --service develop-websocket  --load-test -n 60 --concurrency 5 --all-suppliers
pocket-relay-miner relay grpc      --localnet --service develop-grpc       --load-test -n 60 --concurrency 5 --all-suppliers
```

The run logs `round-robining across session suppliers` with the supplier
count. For WebSocket the supplier is pinned at the handshake, so the pool opens
`max(--concurrency, suppliers)` connections and assigns suppliers to them
round-robin; for HTTP/gRPC the supplier rotates per request.

The supplier list is read once, when the run starts. Each new run follows the
current session; a load test that outlives its session keeps addressing the
suppliers it started with.

### Flag rules and limits

The CLI rejects, before sending anything:

- `--rps`, `--concurrency`, or `-n` greater than 1 without `--load-test`;
- `--batches` outside `stream`, and `-n` greater than 1 or `--load-test` with
  `stream`;
- `--grpc-method`/`--grpc-request-hex` outside `grpc`, combined with
  `--payload`, or `--grpc-request-hex` without `--grpc-method`;
- more than one key source (see [Providing keys](#providing-keys-without-putting-hex-on-the-command-line));
- `--concurrency` outside 1–1000, `-n` above 1,000,000, `--rps` above 10,000,
  `--timeout` outside 1–3600 seconds.

`--verbose` switches the CLI's own log to debug level.

## WebSocket options

- `--ws-handshake v1|v2` (default `v2`) picks the handshake shape. `v2` names
  the supplier in the `Pocket-Supplier-Address` handshake header; `v1` omits
  it, so the relayer takes the supplier from the first relay request on the
  connection. Any value other than `v1` behaves as `v2`.
- `--ws-case <name>` runs **one** adversarial case instead of a relay, and
  asserts what the relayer did: the command exits non-zero when the relayer
  does not behave as expected.

  | `--ws-case` | What the client does | What the relayer must do |
  |---|---|---|
  | `hang` | connects and sends nothing | close with `1013` once the first-frame deadline passes (waits up to `--timeout`) |
  | `garbage` | sends bytes that are not a relay request | close with `4001` |
  | `oversized` | sends a frame past the inbound size cap | close with `1009` |
  | `no-session-header` | sends a relay with no session header | close with `4001` |
  | `supplier-change` | serves one relay, then names a different supplier on the same connection | close with `4001` |
  | `subscribe` | sends one request that makes the backend push 5 responses | deliver all 5, each signed |
  | `abrupt-disconnect` | drops a served connection without a close frame | keep serving: a fresh connection gets its relay |
  | `backend-abrupt-close` | makes the backend drop its connection with no close frame | close the client connection with a valid close code (`1001`), never the reserved `1006` |

  `subscribe` and `backend-abrupt-close` rely on parameters the localnet demo
  backend understands; against another backend, give `subscribe` a real
  subscription request with `--payload`.

```bash
pocket-relay-miner relay websocket --localnet --service develop-websocket --ws-case garbage
pocket-relay-miner relay websocket --localnet --service develop-websocket --ws-handshake v1
```

## Simulated relays (`--simulate`)

`--simulate` fires a relay that the relayer serves end to end but never charges,
verified against identities pinned in the relayer config instead of on-chain
state — useful for health checks. It uses the flags on this page plus
`--sim-key-id`, `--sim-app-pubkey` and `--sim-gateway-pubkeys`; see
[../SIMULATED_RELAYS.md](../SIMULATED_RELAYS.md) for the relayer config, the
flags, and the response codes.

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http --simulate --sim-key-id sim-http
```

## The `[::1]:8180` gotcha

After a relayer pod restart, Tilt sometimes re-binds the `:8180` port-forward
to IPv6-only. If a run fails with `connection refused` on `127.0.0.1:8180`,
point the CLI at the IPv6 loopback explicitly:

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --relayer-url "http://[::1]:8180"
```

## Testing against beta or mainnet (beyond localnet)

`--localnet` is a convenience for the Tilt environment: it fills in a set of
flags so you don't have to. On a real network you supply those flags yourself,
with **your own staked application**. Here is exactly what `--localnet`
substitutes, and what to pass instead:

| What `--localnet` sets for you | On beta / mainnet you pass |
|---|---|
| app key, auto-selected per service | YOUR staked app's key — via `--app-key <name>` (keyring) or `--keys-file` (see below), or `--app-priv-key <hex>` for throwaway testing |
| gateway key | the gateway's key — `--gateway-key <name>` / `--keys-file`, or `--gateway-priv-key <hex>` — only if you sign via a delegated gateway |
| `--node localhost:9090` | `--node <host:port>` — a Shannon full node gRPC endpoint (add `--grpc-tls` for a TLS endpoint on `:443`, e.g. beta/mainnet) |
| `--chain-id poktroll` | `--chain-id <id>` — the target network's chain id (Shannon mainnet: `pocket`). The CLI requires it; the value is not currently used to sign or route the relay |
| `--relayer-url http://localhost:8180` | `--relayer-url <url>` — your own relayer deployment |
| `--supplier <localnet supplier>` | `--supplier <addr>` or `--all-suppliers` (see below) |

### Who signs: app key vs gateway key

`--app-priv-key` always identifies the **application** — the CLI derives the app
address from it and builds the relay's delegation ring from that app. What
actually *signs* the relay depends on whether you add a gateway key:

- **App mode** (`--app-priv-key` only): the application signs its own relays.
  Use this when you hold the app's key directly.
- **Gateway mode** (`--app-priv-key` + `--gateway-priv-key`): the gateway signs
  on behalf of the app — the same delegated model a gateway uses in production.
  The app must have **delegated to that gateway on-chain**, or the ring
  signature is rejected. Here `--app-priv-key` only names the app (to fetch it
  and build the ring); the gateway key does the signing.

Either way, the application must be **staked for the `--service`** on the target
network, or the relay is rejected.

### Providing keys without putting hex on the command line

Raw `--app-priv-key`/`--gateway-priv-key` hex is visible in your shell history
and to `ps`, so it is for throwaway/localnet testing only. For real keys, use one
of the two secure sources instead — both resolve the key in memory:

- **Keyring by name** (same keyring the miner/relayer and `pocketd` use; the
  backends, the directory rule and the passphrase are documented once in
  [../SUPPLIER_KEYS.md](../SUPPLIER_KEYS.md)).
  Supported backends are `file` and `test`; `os` and `memory` are refused, and
  `--keyring-dir` is the directory CONTAINING the keyring, not the keyring
  itself (the `file` backend reads `<dir>/keyring-file`):

  ```bash
  # file backend: the passphrase is read from stdin, so pipe it in rather than
  # typing it where it can land in shell history.
  read-passphrase-from-your-secret-manager | \
    pocket-relay-miner relay jsonrpc --service <svc> \
      --keyring-backend file --keyring-dir ~/.pocket \
      --app-key <app-key-name> --gateway-key <gateway-key-name> ...
  ```

- **Keys file** (YAML): exactly one key under `applications`, and at most one
  under `gateway` (or `gateways`). More than one of either is rejected, since
  there is no selector.

  ```yaml
  # keys.yaml
  applications:
    - <app-private-key-hex>
  # Omit this section for app-only signing.
  gateway:
    - <gateway-private-key-hex>
  ```

  ```bash
  pocket-relay-miner relay jsonrpc --service <svc> --keys-file keys.yaml ...
  ```

Pick **one** source per run — combining `--keys-file`, `--app-key`/`--gateway-key`,
and `--app-priv-key`/`--gateway-priv-key` is rejected.

### Choosing a supplier

A relay is addressed to one supplier operator in the (app, service) session:

- `--all-suppliers` queries the current session and round-robins across every
  supplier in it (a single relay picks one at random) — the easiest option. The
  list is read once, when the run starts.
- `--supplier <addr>` pins one supplier. Find valid operators by querying the
  session for your (app, service), or just start with `--all-suppliers` and
  read the addresses it logs.

### Example (mainnet, gateway mode)

```bash
pocket-relay-miner relay jsonrpc \
  --service <your-service-id> \
  --keys-file keys.yaml \
  --node <fullnode-grpc-host:port> --grpc-tls \
  --chain-id pocket \
  --relayer-url <your-relayer-url> \
  --all-suppliers --load-test -n 100 --concurrency 10
```

Here `keys.yaml` carries both the app and gateway keys (see above); swap it for
`--app-key`/`--gateway-key` if the keys live in a keyring. Only `--service`, a key
source, `--node`, and `--chain-id` are strictly required (the CLI errors without
them). `--relayer-url` defaults to `http://localhost:8080` (the relayer's default
`listen_addr`), so set it for a remote target; `--supplier` / `--all-suppliers`
is needed for the relay to reach a real supplier.

> **Handling real keys.** Prefer `--keys-file` or `--app-key`/`--gateway-key`
> (keyring) so private keys never appear in your shell history or in `ps`; raw
> `--app-priv-key`/`--gateway-priv-key` hex is for throwaway/localnet testing
> only. This CLI is a **testing tool** — for production traffic, use a gateway.
> Keep key files `chmod 600` and out of version control.

## Verifying the relays landed (claims / proofs)

A successful relay at the CLI only proves the relayer signed and served it. To
confirm it flowed through the miner into a **claim** (and a **proof** where
required), inspect the submission tracking in Redis after the session's claim
window closes (see [CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) for
window timing).

The `redis` commands connect to `redis://localhost:6379` unless told otherwise,
which is the Tilt-proxied Redis on localnet. Anywhere else, pass `--redis <url>`
or `--config <relayer-or-miner-config>` (the config also carries the key
namespace).

```bash
# Claim/proof status per session for one supplier
pocket-relay-miner redis submissions --supplier <addr>

# Only the failures
pocket-relay-miner redis submissions --supplier <addr> --failed-only

# Session lifecycle state, SMST tree, and the supplier registry
pocket-relay-miner redis sessions  --supplier <addr>
pocket-relay-miner redis smst      --session <session_id>
pocket-relay-miner redis supplier  --list

# WAL depth: did the miner actually consume what the relayer published?
# PENDING should fall to 0 once the miner drains the stream.
pocket-relay-miner redis streams   --supplier <addr>
```

The `submissions` output shows, per session-end height and service, the
`CLAIM_STATUS`, `PROOF_STATUS`, `RELAYS`, and compute units.

`redis streams` is the fastest way to tell a relayer problem from a miner one:
relays that the relayer never published leave the stream empty, while relays it
published but the miner never drained leave `PENDING` stuck above zero.

### Fewer on-chain relays than you sent?

`RELAYS` in a claim can be lower than the number you fired. Two relays that
hash to the same SMST leaf (byte-identical relays — request and response)
collapse into one on-chain relay — this is the dedup / anti-replay design, not
a lost relay. The CLI generates a fresh ring signature per request specifically
to avoid this; see [CLAIM_LEAF_MODEL.md](../CLAIM_LEAF_MODEL.md) for the full
model.

## Worked example: full economic cycle for one protocol

```bash
# 1. Note the current height and send a round-robin batch
curl -s http://localhost:26657/status | jq -r '.result.sync_info.latest_block_height'
pocket-relay-miner relay grpc --localnet --service develop-grpc \
  --load-test -n 60 --concurrency 5 --all-suppliers

# 2. Wait for the session end + claim/proof windows to pass (see CLAIM_PROOF_LIFECYCLE.md),
#    then confirm every supplier claimed and proved:
for s in $(pocket-relay-miner redis supplier --list | awk '/^pokt/{print $1}'); do
  pocket-relay-miner redis submissions --supplier "$s" | grep develop-grpc
done
```

A healthy run shows `✓ SUCCESS` claim + proof for each supplier that received
relays, and the on-chain `EventClaimSettled` for the session marks the claim
`VALIDATED` with the correct mint.

## See also

- [TILT.md](TILT.md) — bringing up the localnet and the port map.
- [../SIMULATED_RELAYS.md](../SIMULATED_RELAYS.md) — relays that are served but never charged (`--simulate`).
- [../CLAIM_PROOF_LIFECYCLE.md](../CLAIM_PROOF_LIFECYCLE.md) — claim/proof windows and the inclusion reconciler.
- [../REDIS.md](../REDIS.md) — the `redis` debug subcommands in depth.
