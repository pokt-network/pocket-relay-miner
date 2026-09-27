## Claim leaf model — why some relays don't get paid

This document explains **when two relays collapse into one leaf in the SMST**
(and therefore one `num_relays` on-chain) and what an operator can do about
it. It exists because the metric `ha_miner_relays_added_to_smst_total` counts
attempted writes, not distinct billable leaves, and the difference between
the two can surprise operators during incident review.

## The invariant

A relay's SMST key is `RelayHash = sha256(Marshal(Relay{Req, Res}))`.

Two relays with **identical marshaled bytes** produce an identical key, land
at the same position in the SMST, and **the second write overwrites the
first**. Exactly one leaf exists for them in the claim. The on-chain
`EventClaimCreated.num_relays` reflects distinct leaves, not writes.

This is not a bug. It is the protocol's anti-replay safeguard — without it,
a malicious supplier could replay one legitimate relay an arbitrary number
of times and inflate claims.

## What varies across legitimate concurrent requests (so they don't collapse)

1. **Per-request ring signature** — `ring-go` calls
   `curve.NewRandomScalar()` twice per sign, so any caller that signs the
   request **per inbound message** gets distinct bytes, distinct hashes,
   distinct leaves. This is what PATH does for JSON-RPC: every HTTP POST
   goes through `buildAndSignRelayRequest`. Empirically: 10 byte-identical
   `eth_blockNumber` POSTs through PATH yield 10 unique dedup entries and
   10 leaves in the claim.

2. **Per-event response payload (`PayloadHash`)** — for WebSocket
   subscriptions the request is signed once, but each backend event
   produces a different response body → different `PayloadHash` → different
   signable bytes → different supplier signature → different relay bytes.
   Empirically: 1 subscription fanning out to 100 distinct events yields
   `num_relays=100` on-chain.

## When relays DO collapse

Two shapes, both protocol-correct:

1. **Cached/replayed signed requests** — the `pocket-relay-miner relay
   jsonrpc --load-test` tool used to build one signed request and send it N
   times. That produced 1–2 leaves for N requests. Fixed by `cmd/relay/{http,
   websocket, grpc}.go`: each worker now calls `BuildRelayRequest` itself,
   matching production sign-per-request semantics.

2. **A subscription whose backend events are byte-identical** — e.g. a
   heartbeat or a keepalive ping that always returns `{"result":"pong"}`.
   Even with the per-event response path intact, every `PayloadHash` is
   `sha256("pong")` and the supplier signature is deterministic
   (`cryptotypes.PrivKey.Sign` uses RFC 6979), so every relay marshals
   identically and the SMST keeps one leaf. Under today's protocol you
   **do not get paid per event for these**. This is intentional and
   indistinguishable from a supplier trying to replay-inflate.

## How to observe it

Three metrics in `miner/metrics.go` partition the question:

| Metric                                | Meaning                                                                 |
|---------------------------------------|-------------------------------------------------------------------------|
| `ha_miner_relays_added_to_smst_total` | `UpdateTree` CALLS that succeeded — counts attempts, not unique leaves. |
| `ha_miner_claim_num_leaves`           | Distinct leaves in the claim (matches on-chain `num_relays`).           |
| `ha_miner_claim_relay_attempts`       | Session coordinator `RelayCount` at claim time.                         |
| `ha_miner_claim_leaf_collapse_total`  | Claims where leaves < attempts (dedup collapse fired).                  |

A healthy service has `claim_leaf_collapse_total == 0`. When it ticks up,
the matching session also emits a `WARN` with `claim_leaf_count` and
`coordinator_relay_count` so you can find the culprit without a PromQL
excursion.

**A tick means one thing for certain and does not say why: the claim carries
fewer leaves than the relays we counted while serving, so some of that work
will not be billed.** There are at least two causes and they need opposite
responses:

- **Byte-identical relays collapsing onto one SMST key.** Benign, and the case
  this counter was originally written for: a load generator replaying the same
  payload, or a `ping`/`pong` subscription, produces relays that share a
  RelayHash. This is what made the "1000 → 3 on-chain" gap look like a miner
  bug in April 2026; it wasn't.
- **Relays that reached the coordinator's counter but not the flushed tree.**
  Real unbilled work. Measured 2026-08-19: five claims where the coordinator
  had counted 4 and the root held 1, during a supplier-lease handoff right
  after a fleet bring-up. Fifteen relays served and never paid, with
  `claim_success` and `proof_success` both true on all 94 claims.

So do not assume the tester. Check whether the run had distinct payloads: if it
did, the collapse is unbilled work, not dedup.

## Future options (not in this repo alone)

For `PING / PONG 50 times = 50 claims` (the user scenario that needs work
today's protocol can't give you), the fix is a protocol change: add a
`Nonce` or `AttestationSequence` field to `RelayResponseMetadata` in
poktroll, verified on-chain as monotonically increasing per `(supplier,
session)`. That gives byte-distinctness per event without gateway
cooperation and without weakening the replay safeguard. This repo cannot
ship that alone — it needs a coordinated chain upgrade. Until then, the
safe rule is: **one billable relay per distinct (request bytes, response
bytes) pair**.
