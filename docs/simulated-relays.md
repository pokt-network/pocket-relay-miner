## Simulated Relays

Simulated relays let you exercise a running relayer end-to-end — signature
validation, service routing, the **real backend round-trip**, and response
signing — **without** minting a claimable relay and **without** needing any
on-chain application or session state.

A simulated relay is _almost 100% a real relay_. It travels the same
transports and is signed with a **real ring signature** (exactly like a
PATH app/gateway relay). The only differences from a paid relay are:

- it is verified against a ring **pinned in the relayer config** instead of
  read from chain, so it needs no chain access on either side;
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
  into an error).
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
pubkeys.

Provisioning an identity:

1. Choose (or generate) a dedicated **app** keypair and a dedicated **gateway**
   keypair for simulation. Keep the private keys in your health-check / operator
   tooling — they never go in the relayer config.
2. Put the two **public** keys (hex, compressed secp256k1) in `app_pubkey_hex`
   and `gateway_pubkeys_hex`.
3. Pick a `key_id` and set `enabled: true`.

### The header

A simulated relay is signaled by one header/metadata field, carried on each
transport's native channel (the same way `Rpc-Type` already is):

- HTTP (jsonrpc, cometbft, rest/stream) and WebSocket handshake:
  `Pocket-Simulation-Key-Id: <key_id>`
- gRPC metadata: `pocket-simulation-key-id: <key_id>`

The value is the `key_id` of the pinned identity to verify against. It is not a
secret (it travels in plaintext); the ring signature is the actual gate.

### Firing a simulated relay with the CLI

The `relay` CLI builds a locally-ringed simulated request (no chain queries)
and sends it directly to the relayer. Supply the simulation flags plus a
supplier that is loaded on the relayer:

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

### Verifying it is never mined

A simulated relay must produce **no** mining state — that is what guarantees it
is never claimed, and therefore never settled or paid for. To confirm, fire a
burst of simulated relays and check that no claim/session/WAL state was created
— only the simulated metric moves:

```bash
# Fire a burst of simulated relays (above), then inspect Redis:
redis-cli --scan --pattern 'ha:miner:sessions:*' | wc -l   # unchanged by simulation
redis-cli --scan --pattern 'ha:smst:*'           | wc -l   # unchanged by simulation
redis-cli --scan --pattern '*simv1*'             | wc -l   # 0 — a synthetic sim session never persists
redis-cli XLEN ha:relays:<supplier>                        # WAL not grown by simulation
```

A claim is built from an SMST tree; a simulated relay creates no SMST tree, so
it is **structurally impossible** for simulated traffic to produce a claim or a
proof — not merely suppressed.

### Metrics

Simulated relays increment only their own metric and never the real-relay
counters:

- `ha_relayer_simulated_relays_total{transport, service, supplier, result}` —
  `result` is `success` or a rejection reason (`rate_limited`, `verify_failed`,
  `replay_rejected`, `identity_mismatch`, `service_unknown`,
  `service_not_allowed`, `supplier_not_loaded`, `sign_failed`, `backend_error`,
  `meter_degraded`, `dedup_unavailable`). `key_id` is deliberately **not** a
  label.
- `ha_relayer_simulated_relay_duration_seconds{transport, service}` —
  end-to-end latency of simulated relays.

To confirm isolation, watch that a burst of simulated relays moves
`ha_relayer_simulated_relays_total` while `ha_relayer_relays_served_total` and
the other real counters stay flat.

### Rotation and revocation

- Disable one identity without touching others: set its `enabled: false` (or an
  elapsed `not_after`) and reload.
- Rotate a leaked key: generate a new keypair, pin the new public keys under a
  new `key_id` (or replace the existing one), and remove the old identity.
- The `max_rps` cap (default 5) bounds how much a leaked key can do before you
  revoke it.
