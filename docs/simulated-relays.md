## Simulated Relays

Simulated relays let you exercise a running relayer end-to-end — signature
validation, service routing, the **real backend round-trip**, and response
signing — **without** minting a claimable relay and **without** needing any
on-chain application or session state.

A simulated relay is _almost 100% a real relay_. It travels the same
transports and is signed with a **real ring signature** (exactly like a
PATH app/gateway relay). The only differences from a paid relay are:

- it is verified against a ring **pinned in the relayer config** instead of one
  read from chain, so **the relayer needs no chain access to admit it**, and no
  application has to be staked;
- it is **never metered** against the application's stake allowance and
  **never published** to the WAL, so it never becomes part of a claim — and a
  relay that is never claimed is never settled and never paid for;
- it is counted in its **own** metrics and never touches the counters that
  measure real traffic.

Because it is a genuine request to your real backend, a simulated relay tells
you whether the whole path is healthy _right now_ — which is exactly what a
health check or an infrastructure smoke test needs.

### Who this is for

**Operators / NodeRunners, testing their own infrastructure.** That is the whole
audience. Simulated relays are an operator-side capability: you enable them on
your own relayer, against identities whose private keys only you hold. They are
not a surface exposed to third parties — no gateway, application, or external
integrator can fire one against your relayer.

Two ways operators use it:

- **By hand** — validate your backends, supplier keys, and transports before or
  while taking real traffic.
- **Automated, from tooling you deploy yourself** (e.g. Igniter) — wire
  simulated relays as health checks that prove "this relayer, with supplier X
  loaded, can serve service Y right now", without paying for relays.

### How it works (the three zones)

Every relay the relayer serves goes through three zones. A simulated relay
runs the **same** data path as a real one; only Admission and Accounting
differ:

1. **Admission** — for a simulated relay this is: a rate-limit gate, a
   pinned-ring signature check, an identity binding check, and a freshness
   check. (A real relay's admission is the on-chain ring + session + reward
   checks.)
2. **Data path** — decode → route → **backend round-trip** → sign response →
   respond. **Identical** for simulated and real relays.
3. **Accounting** — a real relay is **metered** (the relayer tracks it against
   the application's stake allowance, to decide what it may still serve) and
   **published to the WAL**, so the miner can fold it into an SMST tree and
   submit a claim. The actual burn/payment happens later, on-chain, at
   settlement. A simulated relay does **neither**: it is never metered and
   never published, so no claim is ever built for it and nothing is ever
   settled. It runs a non-mutating meter health probe and records a
   simulated-relay metric instead.

A simulated relay is always **admitted before the backend is called**, even on
a service configured for `optimistic` validation (which serves real relays
before validating). Admission is a simulated relay's only authorization, so it
must run first.

### Security model — all trust is relayer-side

Every control is decided by the relayer against its **own** state (config,
clock, shared Redis). Nothing depends on the honesty of the caller.

- **Off by default.** `simulation.enabled` defaults to `false`. When off, the
  simulation header is **ignored** (a stray header never turns a real relay
  into an error), and the `identities` list is neither validated nor loaded —
  a leftover or half-filled simulation block cannot affect a relayer that has
  the feature switched off.
- **Config allowlist.** Only pinned, enabled, unexpired identities are
  accepted, selected by a `key_id`.
- **Real ring signature over an operator-controlled ring.** The caller must
  sign correctly, like a real PATH relay. The deterministic ring-padding
  "placeholder" key is **forbidden** as a pinned member (its private key is
  publicly derivable), so forgery requires a private key the operator actually
  holds. **Treat simulation gateway private keys as gateway-grade secrets, and
  use dedicated identities — never the keys of a revenue-generating app.**
- **Identity binding.** The request's application address and service must
  match the pinned identity.
- **Freshness + replay protection.** The signed session id embeds a timestamp;
  the relayer rejects anything outside a short window and de-duplicates the
  signature across the HA fleet (shared Redis) so a captured request cannot be
  replayed.
- **Rate limited.** A per-identity request rate cap plus a global concurrency
  cap bound abuse and the blast radius if a key ever leaks.

This is a strong barrier against casual misuse and a faithful health-check
surface. It is **not** a defense against an attacker who obtains a pinned
identity's private key; the rate limit and the per-identity kill switch bound
that risk. A simulated response carries a synthetic session and is never
claimable — no off-chain system should treat it as proof of paid service.

### Configuration

Add a `simulation` section to the relayer config. Store **public keys only** —
never private keys.

```yaml
simulation:
  enabled: true                 # master switch (default false)
  max_concurrent: 32            # global concurrency cap across all sim traffic
  freshness_window_seconds: 30  # accepted clock skew for a request's timestamp
  identities:
    - key_id: "igniter-healthcheck"   # sent as the Pocket-Simulation-Key-Id header
      enabled: true                   # per-identity switch (required to activate)
      not_after: "2026-12-31T00:00:00Z" # optional expiry, for rotation
      max_rps: 5                      # per-identity request rate cap
      app_pubkey_hex: "02abc..."      # application pubkey (ring index 0)
      gateway_pubkeys_hex: ["03def..."] # operator-held gateway pubkey(s); NOT the placeholder
      allowed_services: ["develop-http"] # services this identity may target (empty = all configured)
```

Config validation **rejects** a config that pins the ring-padding placeholder
key, a duplicate `key_id`, a malformed pubkey, or an identity with no gateway
pubkeys. "Malformed" includes a pubkey that is the right length and has a valid
`02`/`03` compressed prefix but whose x coordinate is **not a point on the
secp256k1 curve** — the shape you get from pasting a documentation placeholder
into a real config. The rejection names the identity and the field, e.g.:

```
simulation.identities[0] (key_id=my-sim-identity): gateway_pubkeys_hex[0]: simulation identity: malformed pubkey hex: pubkey is not a valid secp256k1 curve point: invalid public key: x coordinate bbbb...bbbb is not on the secp256k1 curve
```

Validation runs only when `simulation.enabled` is `true`; a disabled block is
skipped wholesale. Turning the feature on with no identities pinned is itself
rejected — an enabled block that serves nothing would reject every simulated
relay as an unknown `key_id`, which looks identical to a forged signature in
the metrics.

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
simulation identity. Instead, both `relayer validate` and relayer startup warn
and name the `key_id`:

```
warning: simulation identities are past their not_after and will reject every relay: igniter-healthcheck
```

`relayer validate` reports this whether or not `simulation.enabled` is set — a
dead identity is worth knowing about *before* the deploy that switches the
feature on.

### The header

A simulated relay is signaled by one header/metadata field, carried on each
transport's native channel (the same way `Rpc-Type` already is):

- HTTP (jsonrpc, cometbft, rest/stream) and WebSocket handshake:
  `Pocket-Simulation-Key-Id: <key_id>`
- gRPC metadata: `pocket-simulation-key-id: <key_id>`

The value is the `key_id` of the pinned identity to verify against. It is not a
secret (it travels in plaintext); the ring signature is the actual gate.

### Firing a simulated relay with the CLI

The `relay` CLI **builds** a locally-ringed simulated request — the ring comes
from the pinned public keys, so **signing** needs no chain query and no staked
application.

The CLI itself still connects to a node, and it is worth being precise about
why: after the relay returns, it verifies the **supplier's signature** on the
response, which means resolving that supplier's public key from chain. Signing a
simulated relay needs no chain; checking the answer does. The CLI always
verifies and offers no flag to skip it — a forged response is exactly what you
want a health check to catch.

Supply the simulation flags plus a supplier that is loaded on the relayer:

```bash
pocket-relay-miner relay jsonrpc \
  --localnet \
  --service develop-http \
  --supplier pokt1<supplier-operator-address> \
  --simulate --sim-key-id igniter-healthcheck
```

Works across all five modes: `jsonrpc`, `rest`/`stream`, `cometbft`, `grpc`,
`websocket`. The app and gateway public keys used to build the ring are derived
from the app/gateway keys the CLI already resolves (the same keys whose public
halves you pinned in the relayer config).

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

Localnet examples (relayer direct on `:8180`, supplier1 loaded):

```bash
SUP=pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj

pocket-relay-miner relay jsonrpc   --localnet --service develop-http      --supplier $SUP --simulate --sim-key-id sim-http
pocket-relay-miner relay cometbft  --localnet --service develop-cometbft  --supplier $SUP --simulate --sim-key-id sim-cometbft
pocket-relay-miner relay grpc      --localnet --service develop-grpc      --supplier $SUP --simulate --sim-key-id sim-grpc
pocket-relay-miner relay websocket --localnet --service develop-websocket --supplier $SUP --simulate --sim-key-id sim-ws

# Streaming (SSE): a stream is one relay whose data is delivered in batches.
# Each batch is supplier-signed; the whole stream is a single relay. Use
# --batches N so the demo backend closes the stream after N events instead of
# streaming forever (a real streaming backend closes on its own).
pocket-relay-miner relay stream    --localnet --service develop-stream    --supplier $SUP --simulate --sim-key-id sim-stream --batches 3
```

### Verifying it yourself

Do not take the claims above on trust — they are checkable in a few minutes, and
you should check them on your own deployment before wiring simulated relays into
anything. The walk-through below is the one used to validate the feature; every
command and every expected result is real output from a localnet run.

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

#### 1. Baseline: is the relayer serving real relays?

Establish this first, or a later rejection will look like simulation's fault
when it is not.

```bash
SUP=pokt19a3t4yunp0dlpfjrp7qwnzwlrzd5fzs2gjaaaj
pocket-relay-miner relay jsonrpc --localnet --service develop-http --supplier $SUP
```

Expect `SUCCESS`, `Signature: VALID`, and a real backend result.

#### 2. The happy paths

Run the five commands from the previous section. Each should return `SUCCESS`,
`Signature: VALID`, and a **real** backend response — a simulated relay hits your
real backend; only the accounting is skipped.

A detail worth noticing in the output: the simulated relay's **`Build Time` is
~1 ms against ~34 ms for the real one**. Those ~33 ms are the chain query the
simulated path does not make, because its ring comes from your config.

#### 3. The unhappy paths — these must be rejected

A feature that only works is half-tested. All three of these are decided by the
relayer against its own state; the caller is never trusted.

```bash
# Unknown key_id: no such identity is pinned
pocket-relay-miner relay jsonrpc --localnet --service develop-http --supplier $SUP \
  --simulate --sim-key-id does-not-exist
# -> 403 simulation rejected: simulation: unknown key_id

# Identity/service mismatch: sim-http's identity aimed at a different service
pocket-relay-miner relay cometbft --localnet --service develop-cometbft --supplier $SUP \
  --simulate --sim-key-id sim-http
# -> 403 simulation rejected: simulation: request application address does not
#    match pinned identity

# Forgery: a cryptographically VALID signature over a ring you have not pinned
pocket-relay-miner relay jsonrpc --localnet --service develop-http --supplier $SUP \
  --simulate --sim-key-id sim-http \
  --gateway-priv-key 1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11 \
  --sim-gateway-pubkeys 02bbbf99abdcddac27350bca272d7146187c091aacfc1c6f90819c9b6daf4fe846
# -> 403 simulation rejected: simulation: ring signature verification failed:
#    ring not in pinned set
```

The forgery case needs **both** flags. Overriding only `--sim-gateway-pubkeys`
makes the CLI sign with its real gateway key over a ring that key is not a member
of, so signing fails locally (`failed to find given key in public key set`) and
the request never reaches the relayer — that tests the client, not the defence.
Passing the matching private key produces a genuine signature over a genuine
ring, which is precisely what the relayer must refuse.

#### 4. Prove nothing was charged

This is the claim that matters. Snapshot the mining state, fire a burst, and
compare.

```bash
# BEFORE
redis-cli --scan --pattern 'ha:relays:*'         | wc -l
redis-cli --scan --pattern 'ha:smst:*'           | wc -l
redis-cli --scan --pattern 'ha:miner:sessions:*' | wc -l

# 25 simulated relays
for i in $(seq 1 25); do
  pocket-relay-miner relay jsonrpc --localnet --service develop-http --supplier $SUP \
    --simulate --sim-key-id sim-http >/dev/null 2>&1 && echo -n "." || echo -n "x"
done; echo

# AFTER — the first three must be IDENTICAL
redis-cli --scan --pattern 'ha:relays:*'         | wc -l
redis-cli --scan --pattern 'ha:smst:*'           | wc -l
redis-cli --scan --pattern 'ha:miner:sessions:*' | wc -l
redis-cli --scan --pattern '*simv1*'             | wc -l   # must be 0
redis-cli --scan --pattern 'ha:sim:replay:*'     | wc -l   # will be 25 — see below
```

A real run: `ha:relays` 15 → **15**, `ha:smst` 0 → **0**, `ha:miner:sessions`
4 → **4**, `*simv1*` = **0**, after 25 simulated relays.

**`ha:sim:replay:*` growing is correct, not a leak.** It is the replay-dedup set
— one entry per simulated signature, shared across the fleet so a captured
request cannot be replayed once to each replica. It expires on its own (TTL =
2× the freshness window) and is not mining state: no WAL, no tree, no claim.

#### 5. Confirm the metric isolation

```bash
# On localnet the relayer serves its own metrics on :9190 (9091 is Prometheus).
curl -s localhost:9190/metrics | grep -E '^ha_relayer_(relays_received_total|simulated_relays_total)'
```

A real run, after 27 simulated and 2 real relays:

```
ha_relayer_relays_received_total{rpc_type="jsonrpc",service_id="develop-http"} 2
ha_relayer_simulated_relays_total{result="success",service="develop-http",...} 27
```

The simulated traffic never touched the counter that measures real load. With
more than one replica, aggregate across them instead:

```bash
curl -sG localhost:9091/api/v1/query \
  --data-urlencode 'query=sum(ha_relayer_simulated_relays_total) by (result)'
```

#### 6. Prove the check above is not blind

The most important step, and the easiest to skip. "Nothing changed" is worthless
unless the same counters *do* change when a relay is genuinely mined:

```bash
pocket-relay-miner relay jsonrpc --localnet --service develop-http --supplier $SUP
sleep 2
redis-cli --scan --pattern 'ha:smst:*'           | wc -l
redis-cli --scan --pattern 'ha:miner:sessions:*' | wc -l
```

A real run: `ha:smst` 0 → **2** and `ha:miner:sessions` 4 → **6**. One real relay
moves them; 25 simulated ones did not.

#### Why it is impossible, not merely suppressed

A claim is built from an SMST tree. A simulated relay is never published to the
WAL, so no tree is ever built, so there is nothing for a claim to be made from.
The absence of mining state is not a rule the code remembers to follow — there
is no code path from a simulated relay to a claim.

### Metrics

Simulated relays increment only their own metric and never the real-relay
counters:

- `ha_relayer_simulated_relays_total{transport, service, supplier, result}` —
  `result` is `success` or a rejection reason (`rate_limited`, `verify_failed`,
  `replay_rejected`, `identity_mismatch`, `service_unknown`,
  `service_not_allowed`, `supplier_not_loaded`, `sign_failed`, `backend_error`,
  `meter_degraded`, `dedup_unavailable`). `key_id` is deliberately **not** a
  label: it would be caller-controlled and unbounded, and a metric label is a
  bad place to learn that.
- `ha_relayer_simulated_relay_duration_seconds{transport, service}` —
  end-to-end latency of simulated relays.

**`verify_failed` is the catch-all**, and it is wider than its name suggests. It
covers an unknown or disabled `key_id`, an expired identity, a stale timestamp,
a malformed session id, **and** a bad signature. So a spike in `verify_failed`
is not necessarily someone forging signatures — the far likelier causes are a
`key_id` you removed, an identity that hit its `not_after`, or clock skew
between your health checker and the relayer. The other results are specific;
this one needs the relayer's logs to narrow down.

To confirm isolation, watch that a burst of simulated relays moves
`ha_relayer_simulated_relays_total` while `ha_relayer_relays_received_total`,
`ha_relayer_relays_served_total` and the other real counters stay flat.

### Rotation and revocation

- Disable one identity without touching others: set its `enabled: false` (or an
  elapsed `not_after`) and reload.
- Rotate a leaked key: generate a new keypair, pin the new public keys under a
  new `key_id` (or replace the existing one), and remove the old identity.
- The `max_rps` cap (default 5) bounds how much a leaked key can do before you
  revoke it.
