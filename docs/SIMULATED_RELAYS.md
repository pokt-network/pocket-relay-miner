# Simulated Relays

Simulated relays let you exercise a running relayer end-to-end — signature
validation, service routing, the **real backend round-trip**, and response
signing — **without** minting a claimable relay and **without** needing any
on-chain application or session state.

A simulated relay is _almost 100% a real relay_. It travels the same
transports and is signed with a **real ring signature**, exactly like a
gateway relay. The only differences from a paid relay are:

- it is verified against a ring **pinned in the relayer config** instead of one
  read from chain, so **the relayer needs no chain access to admit it**, and no
  application has to be staked;
- it is **never metered** against the application's stake allowance and
  **never published** to the WAL, so it never becomes part of a claim — and a
  relay that is never claimed is never settled and never paid for;
- once the relayer recognises a request as simulated, it is counted only in its
  **own** metrics (see [Metrics](#metrics) for the requests refused before that
  point).

Because it is a genuine request to your real backend, a simulated relay tells
you whether the whole path is healthy _right now_ — which is exactly what a
health check or an infrastructure smoke test needs.

## Who this is for

**Operators / NodeRunners, testing their own infrastructure.** That is the whole
audience. Simulated relays are an operator-side capability: you enable them on
your own relayer, against identities whose private keys only you hold. They are
not a surface exposed to third parties — no gateway, application, or external
integrator can fire one against your relayer.

Two ways operators use it:

- **By hand** — validate your backends, supplier keys, and transports before or
  while taking real traffic.
- **Automated, from tooling you deploy yourself** (e.g. your own health-check
  service) — wire simulated relays as health checks that prove "this relayer,
  with supplier X loaded, can serve service Y right now", without paying for
  relays.

## How it works (the three zones)

Every relay the relayer serves goes through three zones. A simulated relay
runs the **same** data path as a real one; only Admission and Accounting
differ:

1. **Admission** — for a simulated relay this is: a global concurrency gate, a
   pinned-ring signature check, an identity binding check, a freshness and
   replay check, and then a per-identity rate gate. (A real relay's admission
   is the on-chain ring + session + reward checks.)
2. **Data path** — decode → route → **backend round-trip** → sign response →
   respond. **Identical** for simulated and real relays.
3. **Accounting** — a real relay is **metered** (the relayer tracks it against
   the application's stake allowance, to decide what it may still serve) and
   **published to the WAL**, so the miner can fold it into an SMST tree and
   submit a claim. The actual burn/payment happens later, on-chain, at
   settlement. A simulated relay does **neither**: it is never metered and
   never published, so no claim is ever built for it and nothing is ever
   settled. Instead it records a simulated-relay metric and — on `jsonrpc`,
   `cometbft` and gRPC only — runs a non-mutating meter health probe whose
   outcome shows up in that metric's `result` label.

A simulated relay is always **admitted before the backend is called**, even on
a service configured for `optimistic` validation (which serves real relays
before validating). Admission is a simulated relay's only authorization, so it
must run first.

## Security model — all trust is relayer-side

Every control is decided by the relayer against its **own** state (config,
clock, shared Redis). Nothing depends on the honesty of the caller.

- **Off by default.** `simulation.enabled` defaults to `false`. When off, the
  simulation header is **ignored** (a stray header never turns a real relay
  into an error), and the relayer loads no identity. A disabled block never
  blocks startup; if it pins identities, `relayer validate` and relayer startup
  still check them and warn about anything that would be rejected once you
  enable the feature (see [Configuration](#configuration)).
- **Config allowlist.** Only pinned, enabled, unexpired identities are
  accepted, selected by a `key_id`.
- **Real ring signature over an operator-controlled ring.** The caller must
  sign correctly, like a real gateway relay. The deterministic ring-padding
  "placeholder" key is **forbidden** as a pinned member (its private key is
  publicly derivable), so forgery requires a private key the operator actually
  holds. **Treat simulation gateway private keys as gateway-grade secrets, and
  use dedicated identities — never the keys of a revenue-generating app.**
- **Identity binding.** The request's application address must match the
  pinned identity, and its service must be one of the identity's
  `allowed_services` (any service configured on the relayer when the list is
  empty).
- **Freshness + replay protection.** The signed session id embeds a timestamp;
  the relayer rejects anything further than `freshness_window_seconds` from its
  own clock, in either direction, and de-duplicates the signature across the HA
  fleet (shared Redis) so a captured request cannot be replayed.
- **Rate limited.** A per-identity request rate cap plus a global concurrency
  cap bound abuse and the blast radius if a key ever leaks. The per-identity
  cap is charged only **after** a request verifies, so someone who knows a
  `key_id` (it is not a secret) but holds no key cannot spend that identity's
  budget. The global cap is taken before verification.

This is a strong barrier against casual misuse and a faithful health-check
surface. It is **not** a defense against an attacker who obtains a pinned
identity's private key; the rate limit and the per-identity kill switch bound
that risk. A simulated response carries a synthetic session and is never
claimable — no off-chain system should treat it as proof of paid service.

## Configuration

Add a `simulation` section to the relayer config. Store **public keys only** —
never private keys. The field names and defaults match the `simulation` block
of `config.relayer.example.yaml`.

```yaml
simulation:
  # Master switch. Default: false.
  enabled: true
  # Max simulated relays in flight across all identities. Default: 32.
  max_concurrent: 32
  # How far a request's timestamp may be from the relayer's clock, in seconds.
  # Default: 30.
  freshness_window_seconds: 30
  identities:
    # Sent by the client as the Pocket-Simulation-Key-Id header.
    - key_id: "my-sim-identity"
      # Per-identity switch; the identity is served only when this is true.
      enabled: true
      # Optional expiry (RFC3339), for rotation. Unset means never expires.
      not_after: "2027-01-01T00:00:00Z"
      # Per-identity request rate cap. Default: 5.
      max_rps: 5
      # Compressed secp256k1 public key (hex) of the simulated application.
      # Replace with the public half of a key pair you generated.
      app_pubkey_hex: "02397b4351edd177a6e6429aa72a6e5a702372b08b8b48a923d08c97bf5a800c87"
      # Compressed secp256k1 public key(s) (hex) of the gateway(s) that sign.
      # At least one. Never the ring placeholder key.
      gateway_pubkeys_hex:
        - "02c94cceea9d874974cb0ab1e368d805e8eb07a6bc8b1eb4c8edf4d08b65d4800b"
      # Services this identity may target. Empty means every configured service.
      allowed_services:
        - develop-http
```

The two public keys above are valid secp256k1 points whose private halves were
discarded when they were generated: the block passes `relayer validate`, but
nothing can sign for it, so it serves nothing until you put your own keys in.

Config validation **rejects** (`relayer validate` exits `1`, and the relayer
refuses to start) a config that pins the ring-padding placeholder key, an empty
or duplicate `key_id`, a malformed pubkey, an identity with no gateway pubkeys,
an explicit `max_rps` of zero or less after defaults (an unset one defaults to
5), or a `not_after` that is not RFC3339. "Malformed" includes a pubkey that is
the right length and has a valid `02`/`03` compressed prefix but whose x
coordinate is **not a point on the secp256k1 curve** — the shape you get from
pasting a documentation placeholder into a real config. The rejection names the
identity and the field, e.g.:

```
Error: config is INVALID: invalid config: invalid simulation config: simulation.identities[0] (key_id=my-sim-identity): gateway_pubkeys_hex[0]: simulation identity: malformed pubkey hex: pubkey is not a valid secp256k1 curve point: invalid public key: x coordinate bbbb...bbbb is not on the secp256k1 curve
```

Turning the feature on with no identities pinned is itself rejected
(`simulation: enabled but no identities are pinned`) — an enabled block that
serves nothing would reject every simulated relay as an unknown `key_id`, which
looks identical to a forged signature in the metrics.

These checks are enforced only when `simulation.enabled` is `true`. A disabled
block that pins identities is still checked, but only as a warning, so you
learn about a broken identity before the deploy that switches the feature on:

- `relayer validate` prints
  `warning: simulation is disabled, so its config was not checked; it would be REJECTED if you set simulation.enabled: true: <reason>`
  and still exits `0`;
- relayer startup logs a warning,
  `simulation is disabled and its config is invalid: enabling it would prevent startup`.

Provisioning an identity:

1. Choose (or generate) a dedicated **app** keypair and a dedicated **gateway**
   keypair for simulation. Keep the private keys in your health-check / operator
   tooling — they never go in the relayer config.
2. Put the two **public** keys (hex, compressed secp256k1) in `app_pubkey_hex`
   and `gateway_pubkeys_hex`. These must be public halves of key pairs you
   actually generated — the relayer verifies a real ring signature against
   them, so invented or copied-from-docs values can never produce a servable
   identity.
3. Pick a `key_id` and set `enabled: true`.
4. Restart the relayer. The whole `simulation` block — identities,
   `max_concurrent` and `freshness_window_seconds` included — is read once at
   startup; there is no hot reload.

`config.relayer.example.yaml` ships this block with `enabled: false` and the
whole `identities` key commented out, with the field shape shown as a template.
To pin an identity, delete the leading `# ` (hash and one space) from every line
of that template and replace the placeholder keys — do not edit placeholder
values in place.

Lines that are still comments after that single pass carry a second `#` on
purpose: those are the optional fields (`max_rps`, `not_after`,
`allowed_services`), and leaving them commented is what keeps them unset.
Uncomment only the ones you want. In particular, `not_after` is a scheduled
stop: on that date the identity stops being served and your simulated traffic
goes dark, with no other config change.

An identity whose `not_after` has already passed is **not** a startup error —
it loads, and then rejects every relay aimed at it with `simulation: identity
expired (not_after)`. Making it fatal would mean the next restart of a healthy
relayer refuses to boot and takes real, paid traffic down over a dead
simulation identity. Instead, `relayer validate` warns and names the `key_id`:

```
warning: simulation identities are past their not_after and will reject every relay: my-sim-identity
```

`relayer validate` reports this whether or not `simulation.enabled` is set — a
dead identity is worth knowing about *before* the deploy that switches the
feature on. It lists only identities with `enabled: true`. At startup, and only
when the feature is enabled, the relayer logs a warning,
`simulated relay identities are past their not_after: they will reject every relay`,
with the ids in the `key_ids` field.

## The header

A simulated relay is signaled by one header/metadata field, carried on each
transport's native channel (the same way `Rpc-Type` already is):

- HTTP (jsonrpc, cometbft, stream) and the WebSocket handshake:
  `Pocket-Simulation-Key-Id: <key_id>`
- gRPC metadata: `pocket-simulation-key-id: <key_id>`

The value is the `key_id` of the pinned identity to verify against. It is not a
secret (it travels in plaintext); the ring signature is the actual gate. On
WebSocket the header is read once, at the handshake, and every message on that
connection is then admitted as a simulated relay.

## Firing a simulated relay with the CLI

The `relay` CLI **builds** a locally-ringed simulated request — the ring comes
from the pinned public keys, so **signing** needs no chain query and no staked
application. The base flags (`--relayer-url`, `--node`, key sources, load
testing) are the same as for a normal relay and are documented in
[testing/DIRECT_CLI.md](testing/DIRECT_CLI.md).

The CLI itself still connects to a node, and it is worth being precise about
why: after the relay returns, it verifies the **supplier's signature** on the
response, which means resolving that supplier's public key from chain. Signing a
simulated relay needs no chain; checking the answer does. The CLI always
verifies and offers no flag to skip it — a forged response is exactly what you
want a health check to catch.

Supply the simulation flags plus a supplier that is loaded on the relayer:

```bash
pocket-relay-miner relay jsonrpc \
  --service develop-http \
  --keys-file keys.yaml \
  --node <host:port> --chain-id <id> \
  --relayer-url <relayer-url> \
  --supplier <addr> \
  --simulate --sim-key-id my-sim-identity
```

| Flag | Meaning |
|---|---|
| `--simulate` | Fire a simulated relay instead of a chain-backed one. |
| `--sim-key-id <key_id>` | Required with `--simulate`: the pinned identity to verify against. |
| `--sim-app-pubkey <hex>` | App public key for the ring. Default: derived from the resolved app key (`--app-priv-key` / `--app-key` / `--keys-file`). |
| `--sim-gateway-pubkeys <hex>[,<hex>]` | Gateway public key(s) for the ring, comma-separated or repeated. Default: derived from the resolved gateway key. |

`--simulate` also requires `--supplier` (under `--localnet` the default localnet
supplier fills it in, even with `--all-suppliers`), an application key (the CLI
refuses to run without one even when `--sim-app-pubkey` is given), and either a
gateway key or `--sim-gateway-pubkeys`. The ring the relayer checks is the app
public key plus the gateway public key(s) — the same public halves you pinned in
the relayer config — and the CLI signs with the gateway key when it has one.

It works across all five modes: `jsonrpc`, `stream`, `cometbft`, `grpc`,
`websocket`, and with `--load-test` too — keep the rate under the identity's
`max_rps` (default 5), or the excess is refused as `rate_limited`.

**Firing one from your own tooling, in another language:** the CLI is convenient
but not required — a simulated relay is an ordinary signed relay, so anything
that can produce the ring signature can send one.
[`examples/relay-signing/`](../examples/relay-signing/README.md) documents the
signature byte-for-byte and ships working signers in **Node.js**, **Python** and
**Rust**, plus an oracle to verify an implementation of your own. Note that you
need only **one** private key — the signer's, normally the gateway's — plus the
public keys of the ring.

A successful simulated relay returns a **supplier-signed** response containing
the real backend result — the same response shape a paying gateway would
receive.

Each transport uses the app key of its service, so the pinned identity's
`key_id` must match that service. On localnet (`tilt up`) one identity per
`develop-*` service is pre-pinned:

| Mode | Service | `--sim-key-id` |
|---|---|---|
| `jsonrpc` | `develop-http` | `sim-http` |
| `websocket` | `develop-websocket` | `sim-ws` |
| `stream` | `develop-stream` | `sim-stream` |
| `grpc` | `develop-grpc` | `sim-grpc` |
| `cometbft` | `develop-cometbft` | `sim-cometbft` |

Localnet examples (relayer direct on `:8180`; `--localnet` fills in the keys,
the node, the relayer URL and the first localnet supplier):

```bash
pocket-relay-miner relay jsonrpc   --localnet --service develop-http      --simulate --sim-key-id sim-http
pocket-relay-miner relay cometbft  --localnet --service develop-cometbft  --simulate --sim-key-id sim-cometbft
pocket-relay-miner relay grpc      --localnet --service develop-grpc      --simulate --sim-key-id sim-grpc
pocket-relay-miner relay websocket --localnet --service develop-websocket --simulate --sim-key-id sim-ws

# Streaming (SSE): a stream is one relay whose data is delivered in batches.
# Each batch is supplier-signed; the whole stream is a single relay. Use
# --batches N so the demo backend closes the stream after N events instead of
# streaming forever (a real streaming backend closes on its own).
pocket-relay-miner relay stream    --localnet --service develop-stream    --simulate --sim-key-id sim-stream --batches 3
```

## Response codes

What a client sees for each outcome, per transport. The `result` column is the
label on `ha_relayer_simulated_relays_total` (see [Metrics](#metrics)).

| Outcome | HTTP (`jsonrpc`, `cometbft`, `stream`) | gRPC status | WebSocket | `result` |
|---|---|---|---|---|
| Served | `200` with the signed response (a backend `4xx` is signed and returned the same way) | `OK` | signed response frame | `success`, or `meter_degraded` |
| Global concurrency cap (`max_concurrent`) reached | `429` | `RESOURCE_EXHAUSTED` | close `1013` | `rate_limited` |
| Per-identity rate (`max_rps`) exceeded | `429` | `RESOURCE_EXHAUSTED` | close `1013` | `rate_limited` |
| Signature already seen (replay) | `409` | `ALREADY_EXISTS` | close `4001` | `replay_rejected` |
| Replay-dedup store (Redis) unreachable | `503` | `UNAVAILABLE` | close `4001` | `dedup_unavailable` |
| Malformed synthetic session id | `400` | `INVALID_ARGUMENT` | close `4001` | `verify_failed` |
| No supplier address in the request | `400` | refused before admission (see below) | close `4001` | `supplier_not_loaded` |
| Supplier's signing key not loaded on this relayer | `403` | refused before admission (see below) | close `4001` | `supplier_not_loaded` |
| Request names a service the relayer does not configure | refused before admission (see below) | refused before admission (see below) | close `4001` | `service_unknown` |
| Application address does not match the identity | `403` | `PERMISSION_DENIED` | close `4001` | `identity_mismatch` |
| Service not in the identity's `allowed_services` | `403` | `PERMISSION_DENIED` | close `4001` | `service_not_allowed` |
| Unknown, disabled or expired `key_id`; stale timestamp; bad signature | `403` | `PERMISSION_DENIED` | close `4001` | `verify_failed` |
| Backend unreachable or failing | `502`, or the backend's own `5xx` passed through unsigned | `UNAVAILABLE` | — | `backend_error` |
| Response signing failed | `500` | `INTERNAL` | — | `sign_failed` |

The HTTP body of a rejection is JSON, `{"error":"simulation rejected: <reason>"}`
for the admission failures, where `<reason>` is the relayer's error (for
example `simulation: unknown key_id`). A WebSocket rejection closes the whole
connection with the reason `simulation rejected` (or
`simulation concurrency limit reached` / `simulation rate limit reached` for
`1013`).

**Refused before admission.** Some checks run before the relayer looks at the
simulation header, so they answer a simulated request exactly as they would a
real one and are counted in `ha_relayer_relays_rejected_total`, not in the
simulated metric:

- Redis cannot take writes (storage saturated): HTTP `429`, gRPC
  `RESOURCE_EXHAUSTED`, WebSocket handshake `429`.
- Unreadable, oversized or non-relay request body: HTTP `400` / `413`.
- Service not configured on the relayer: HTTP `404`, gRPC `NOT_FOUND`, WebSocket
  handshake `404`. (On WebSocket a later frame can still name an unknown
  service, which is when `service_unknown` appears.)
- gRPC only: no supplier address (`INVALID_ARGUMENT`) or no signing key for that
  supplier (`FAILED_PRECONDITION`).
- WebSocket only: a handshake that names a supplier this relayer holds no key
  for gets HTTP `403` before the upgrade.

## Verifying it yourself

Do not take the claims above on trust — they are checkable in a few minutes, and
you should check them on your own deployment before wiring simulated relays into
anything. The walk-through below runs on the Tilt localnet; each step says what
you should see.

> **Where the localnet identities come from.** `tilt_config.yaml` is user-local
> and gitignored. If its `simulation` block has an `identities` key, that key
> **wins** — it is the file to edit to change an identity's `not_after`,
> `max_rps` or `allowed_services` for a localnet experiment. If the key is absent
> (including a file with no `simulation` block at all, e.g. one created before the
> feature existed), Tilt injects the five `sim-*` localnet defaults listed above,
> so the commands here work either way. An explicit `identities: []` is respected
> rather than filled in, so you can reproduce the "enabled but nothing pinned"
> config error on purpose. Inspect what you ended up with:
> `kubectl get cm relayer-config -o jsonpath='{.data.config\.yaml}' | grep -A20 'simulation:'`
>
> Editing the config **rolls the relayer and miner pods**: their Deployments carry
> a `pocket-relay-miner/config-hash` annotation over the rendered config. This is
> load-bearing, not cosmetic — a mounted ConfigMap change does not restart pods on
> its own, and neither binary re-reads its config after startup (simulation
> identities in particular are not hot-reloaded), so without the annotation a
> config edit would update the ConfigMap and leave the running pods on the old
> config, with no error and no signal.

### 1. Baseline: is the relayer serving real relays?

Establish this first, or a later rejection will look like simulation's fault
when it is not.

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http
```

Expect `SUCCESS`, `Signature: ✅ VALID`, and a real backend result.

### 2. The happy paths

Run the five commands from the previous section. Each should return `SUCCESS`,
`Signature: ✅ VALID`, and a **real** backend response — a simulated relay hits
your real backend; only the accounting is skipped.

A detail worth noticing in the output: the simulated relay's **`Build Time` is
much shorter** than the real one's. The difference is the chain query the
simulated path does not make, because its ring comes from your config.

### 3. The unhappy paths — these must be rejected

A feature that only works is half-tested. All three of these are decided by the
relayer against its own state; the caller is never trusted.

```bash
# Unknown key_id: no such identity is pinned
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --simulate --sim-key-id does-not-exist
# -> HTTP 403: {"error":"simulation rejected: simulation: unknown key_id"}

# Identity/service mismatch: sim-http's identity aimed at a different service
pocket-relay-miner relay cometbft --localnet --service develop-cometbft \
  --simulate --sim-key-id sim-http
# -> HTTP 403: {"error":"simulation rejected: simulation: request application
#    address does not match pinned identity"}

# Forgery: a cryptographically VALID signature over a ring you have not pinned
pocket-relay-miner relay jsonrpc --localnet --service develop-http \
  --simulate --sim-key-id sim-http \
  --gateway-priv-key 1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11 \
  --sim-gateway-pubkeys 02bbbf99abdcddac27350bca272d7146187c091aacfc1c6f90819c9b6daf4fe846
# -> HTTP 403: {"error":"simulation rejected: simulation: ring signature
#    verification failed: ring not in pinned set..."}
```

The private key in the forgery case is the public test-vector gateway key of
[`examples/relay-signing/`](../examples/relay-signing/README.md), and the
public key next to it is its public half. Anyone can sign with it, which is
exactly why it makes a good forger here — never pin it or reuse it anywhere
else.

The forgery case needs **both** flags. Overriding only `--sim-gateway-pubkeys`
makes the CLI sign with its real gateway key over a ring that key is not a member
of, so signing fails locally (`failed to find given key in public key set`) and
the request never reaches the relayer — that tests the client, not the defence.
Passing the matching private key produces a genuine signature over a genuine
ring, which is precisely what the relayer must refuse.

### 4. Prove nothing was charged

This is the claim that matters. Snapshot the mining state and the relayer's
mining counters, fire a burst, and compare. The key patterns below assume the
default `base_prefix: ha`; replace `ha` with yours if you changed it.

```bash
snapshot() {
  redis-cli --scan --pattern 'ha:relays:*'         | wc -l
  redis-cli --scan --pattern 'ha:smst:*'           | wc -l
  redis-cli --scan --pattern 'ha:miner:sessions:*' | wc -l
  # Every real relay the relayer serves moves one of these two: published to
  # the WAL, or skipped because it did not meet the mining difficulty.
  curl -s localhost:9190/metrics | grep -E '^ha_relayer_relays_(published|skipped_difficulty)_total'
}

snapshot   # BEFORE

# 25 simulated relays
for i in $(seq 1 25); do
  pocket-relay-miner relay jsonrpc --localnet --service develop-http \
    --simulate --sim-key-id sim-http >/dev/null 2>&1 && echo -n "." || echo -n "x"
done; echo

snapshot   # AFTER: must be IDENTICAL to BEFORE
redis-cli --scan --pattern '*simv1*'         | wc -l   # must be 0
redis-cli --scan --pattern 'ha:sim:replay:*' | wc -l   # one per simulated relay still inside its TTL — see below
```

Expect 25 dots, an unchanged snapshot, and no key containing `simv1` (the
prefix of a simulated session id).

**`ha:sim:replay:*` growing is correct, not a leak.** It is the replay-dedup set
— one entry per simulated signature, shared across the fleet so a captured
request cannot be replayed once to each replica. The entry is written before
the signature is checked, so a request that then fails verification leaves one
too. It expires on its own (TTL = 2× `freshness_window_seconds`) and is not
mining state: no WAL, no tree, no claim.

### 5. Confirm the metric isolation

```bash
# On localnet the relayer serves its own metrics on :9190 (9091 is Prometheus).
curl -s localhost:9190/metrics | grep -E '^ha_relayer_(relays_received_total|simulated_relays_total)'
```

Expect `ha_relayer_simulated_relays_total{result="success",service="develop-http",...}`
to have grown by the number of simulated relays you fired, while
`ha_relayer_relays_received_total{...,service_id="develop-http"}` grew only by
the real relays you fired. With more than one replica, aggregate across
them instead:

```bash
curl -sG localhost:9091/api/v1/query \
  --data-urlencode 'query=sum(ha_relayer_simulated_relays_total) by (result)'
```

### 6. Prove the check above is not blind

The most important step, and the easiest to skip. "Nothing changed" is worthless
unless the same snapshot *does* change when a relay is genuinely served:

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http
sleep 2
snapshot
```

Expect `ha_relayer_relays_published_total` or
`ha_relayer_relays_skipped_difficulty_total` for `develop-http` to have grown by
one (if neither did, look at `ha_relayer_relays_dropped_total`: the relay was
served but not mined, e.g. because the application's stake is exhausted). When
it is the published one, the miner folds the relay into the session's
SMST tree, so the `ha:smst:*` and `ha:miner:sessions:*` counts also grow the
first time a session receives a mined relay. One real relay moves the snapshot;
25 simulated ones did not.

### Why it is impossible, not merely suppressed

A claim is built from an SMST tree. A simulated relay is never published to the
WAL, so no tree is ever built, so there is nothing for a claim to be made from.
The absence of mining state is not a rule the code remembers to follow — there
is no code path from a simulated relay to a claim.

## Metrics

Once a request is recognised as simulated, it increments only the simulated-relay
metrics, never the real-relay counters. Requests refused before that point
(storage saturated, malformed body, unknown service, and on gRPC a missing
supplier or one with no local signer — see
[Response codes](#response-codes)) are counted like any other refused request in
`ha_relayer_relays_rejected_total`.

- `ha_relayer_simulated_relays_total{transport, service, supplier, result}` —
  `transport` is `jsonrpc`, `rest` (stream), `cometbft`, `grpc` or `websocket`.
  `result` is `success`; `meter_degraded` (served with `200` and a signed
  response, but the non-mutating meter probe failed: the service's cost could
  not be resolved or Redis was unreachable — only `jsonrpc`, `cometbft` and gRPC
  run the probe); or a rejection reason (`rate_limited`, `verify_failed`,
  `replay_rejected`, `identity_mismatch`, `service_unknown`,
  `service_not_allowed`, `supplier_not_loaded`, `sign_failed`, `backend_error`,
  `dedup_unavailable`). `key_id` is deliberately **not** a label: it would be
  caller-controlled and unbounded, and a metric label is a bad place to learn
  that.
- `ha_relayer_simulated_relay_duration_seconds{transport, service}` —
  end-to-end latency of simulated relays over the HTTP transports (`jsonrpc`,
  `cometbft`, `rest`). gRPC and WebSocket simulated relays are not observed
  here.

**`verify_failed` is the catch-all**, and it is wider than its name suggests. It
covers an unknown or disabled `key_id`, an expired identity, a stale timestamp,
a malformed session id, **and** a bad signature. So a spike in `verify_failed`
is not necessarily someone forging signatures — the far likelier causes are a
`key_id` you removed, an identity that hit its `not_after`, or clock skew
between your health checker and the relayer. The other results are specific;
for this one the HTTP and gRPC responses carry the exact reason, and the
relayer's debug logs record it for WebSocket.

To confirm isolation, watch that a burst of simulated relays moves
`ha_relayer_simulated_relays_total` while `ha_relayer_relays_received_total`,
`ha_relayer_relays_served_total` and the other real counters stay flat.

## Rotation and revocation

The relayer reads the `simulation` block once, at startup, so every change
below takes effect when you restart the relayer.

- Disable one identity without touching others: set its `enabled: false` (or an
  elapsed `not_after`) and restart the relayer.
- Rotate a leaked key: generate a new keypair, pin the new public keys under a
  new `key_id` (or replace the existing one), remove the old identity, then
  restart the relayer.
- The `max_rps` cap (default 5) bounds how much a leaked key can do before you
  revoke it.
