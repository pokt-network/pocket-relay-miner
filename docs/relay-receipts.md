## Relay receipts

A relay receipt is a supplier signature that binds **one request** to **one
response**. A caller that asks for one can prove, to itself or to a third
party, that this supplier returned this body in answer to that request.

Nothing is configured. A relayer that has this feature returns a receipt when
asked; one that predates it ignores the request and returns nothing.

### What a receipt proves — and what it does not

The signature already on every `RelayResponse` proves that the supplier
produced that body inside that session. It does not say **which request** the
body answers: `RelayResponseMetadata` carries only a session header and the
signature, and thousands of relays in one session share a session header.

A receipt closes exactly that gap. It does not do anything else:

| | |
|---|---|
| Proves the response answers this request | ✅ |
| Proves the supplier signed it | ✅ |
| Detects a single altered bit in the response | ✅ |
| Proves the relay was mined or reached the chain | ❌ |
| Proves freshness | ❌ |
| Replaces the response signature | ❌ — both are emitted |

On freshness: the request's ring signature makes each pair unique, so a receipt
cannot be transplanted onto a different request. But the same receipt can be
re-presented indefinitely. If that matters for your use case, bind it to
something of your own.

### Asking for one

```
request  →  Pocket-Sign-Receipt: true
response ←  Pocket-Relay-Receipt: v1.<signature_hex>
```

`true` is the only accepted value, case-insensitive. Anything else, including
an absent header, means no — so a caller that does not know about receipts
costs the relayer nothing.

The signature is 64 bytes, so the header value is `v1.` plus 128 hex
characters. An HTTP header cannot carry raw bytes — a `0x0a` inside a signature
would break header framing — so some encoding is mandatory; hex is the one this
project already uses for keys, hashes and signatures elsewhere, and it has no
padding character to explain. The relayer emits lowercase; verifiers accept
either case.

Supported on HTTP unary transports: `jsonrpc`, `rest`, `cometbft`, for both
real and simulated relays. WebSocket and streaming have no per-message header,
and gRPC would need a second wire format; none of them carry receipts today.

### What is signed

```
digest  = sha256( "POKT-RELAY-RECEIPT-v1\0"
                ‖ relayRequest.Meta.Signature      // the ring signature you sent
                ‖ relayResponse.PayloadHash )      // sha256 of the response payload
receipt = supplierOperatorKey.Sign(digest)
```

`‖` is concatenation. Three details are load-bearing:

- **The domain tag is not decoration.** The supplier operator key signs three
  different things: relay responses, receipts, and the `MsgCreateClaim` /
  `MsgSubmitProof` transactions that collect rewards. All three are secp256k1
  signatures over a sha256 digest, and a signature carries no marker saying
  which contract it belongs to. The tag makes the preimage spaces disjoint by
  construction — it begins `0x50`, while a marshaled protobuf in either of the
  other contexts begins `0x0a`.
- **`v1` lives inside the signed bytes**, not only in the header. If what
  enters the digest ever changes, a v1 verifier fails cleanly instead of
  silently verifying the wrong content.
- **Exactly one variable-length field.** The tag is 22 fixed bytes and
  `PayloadHash` is 32, so the ring signature's extent is determined by the
  total length and no two distinct pairs can produce the same preimage. A
  second variable-length field would break that and would need explicit length
  prefixes.

`PayloadHash` covers `POKTHTTPResponse{status_code, header, body}` as
serialized — so the receipt commits to the status code and headers, not only
the body.

The ring signature transitively commits to the whole request, payload and
session header included, so the receipt does too.

### Verifying one

The verifier **rebuilds** the digest from its own copies and checks the
signature against it. It never parses the receipt: a receipt is a check, not a
container.

1. Take `Meta.Signature` from the `RelayRequest` you sent.
2. Take `PayloadHash` from the `RelayResponse` you received. Optionally confirm
   `sha256(Payload) == PayloadHash`.
3. Rebuild `digest` with the domain tag.
4. Verify against the public key of `Meta.SupplierOperatorAddress`.

If any input differs by one bit from what the supplier used, the digest differs
and verification fails. That is the whole property.

Two ways to get this wrong, both of which fail silently:

- **Pass the 32-byte digest to the verifier, not the preimage.** Cosmos
  `secp256k1` hashes its input internally on both sign and verify, so what is
  actually signed is `sha256(sha256(preimage))`.
- **Reject high-S signatures.** Cosmos rejects S above half the curve order. A
  verifier that accepts them is more permissive than the chain — the one
  direction a verifier must never be wrong in.

### When no receipt comes back

Absence is normal, not an error. It means the operator is running a build that
predates the feature. There is no operator-side switch to inspect and no way to
distinguish "does not support it" from "chose not to", because there is nothing
to choose.

A caller that needs receipts decides its own policy — retry elsewhere,
deprioritise the endpoint, record it. That is deliberately the caller's
decision, not something the relayer negotiates.

### Trying it

```bash
# Simulated relay: no staking required, same data path as a real one.
pocket-relay-miner relay jsonrpc --localnet --simulate --sim-key-id sim-http \
  --service develop-http --request-receipt --verbose
```

On success the CLI prints what it verified and against what:

```
Signature: ✅ VALID
Receipt: ✅ VALID (binds this request to this response)
  request signature: 199 bytes (bLSAG ring)
  response payload hash: 035246ede2579f9e...
```

Without `--request-receipt` no header is requested and none comes back. The
response signature is verified either way — that check is unconditional on
every transport.

### Metrics

```
ha_relayer_relay_receipts_total{service_id}
ha_relayer_relay_receipt_errors_total{service_id,reason}
```

`reason` is one of `no_signer`, `sign_failed`, `missing_payload_hash`.

Only relays whose caller asked for a receipt reach either counter, so
`relay_receipts_total` also measures demand for the feature.

There is no latency histogram, deliberately: measuring the receipt on the hot
path would contaminate the very measurement used to price it.

### Cost, and what happens when it fails

One secp256k1 signature per relay that asks for one. Nothing at all for relays
that do not.

If signing fails, the relay is served exactly as it would have been and the
header is simply absent — the same thing a caller sees from an older relayer,
which callers must already handle. An optional extra never drops traffic that
was already served.

### What this does not touch

Mining. Claims and proofs are byte-for-byte unaffected: the receipt is produced
on the synchronous serving path, after the response is signed and before it is
written, and the publish path never sees it.
