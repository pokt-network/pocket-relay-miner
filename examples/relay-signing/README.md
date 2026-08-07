## Signing a Pocket relay in any language

This directory answers one question: **how do you sign and send a Pocket relay
from a language that is not Go?**

It ships a working, verified signer in **Node.js**, **Python** and **Rust**, plus
the Go **oracle** you test your own implementation against.

### You only need ONE private key

This surprises everyone, so it comes first.

A relay is signed by a **ring** of `[application, gateway]`. But a ring is built
from **public** keys, and it is signed by exactly **one** private key — whichever
ring member is doing the signing. Normally that is the **gateway**.

> **You need: the signer's private key, plus the public keys of the ring.**
> You never need the other member's private key.

That is not a shortcut — it *is* what delegation means. When an application
delegates to a gateway, the gateway signs relays on the app's behalf **without
ever holding the app's secret**. The app's public key is public: on-chain for a
real app, pinned in the relayer's config for a simulated one. The signature
proves *a member of the ring* authorized the relay, without revealing which one.

It works in either direction — the app can sign with only the gateway's public
key, exactly as the gateway signs with only the app's:

```
signing with ONLY the gateway private key
  app private key held      : NO  (only its public key)
  ring-go accepts it        : true

mirror image: app signs, no gateway private key
  gateway private key held  : NO  (only its public key)
  ring-go accepts it        : true
```

The Go API says the same thing in its signature —
[`BuildSimulatedRelayRequest`](../../client/relay_client/simulated.go) takes
`appPubKeyHex` and `gatewayPubKeysHex`: **public** keys, plus the one signer it
was constructed with. The CLI's `--app-priv-key` is a convenience that derives
the public key for you; `--sim-app-pubkey` passes it directly.

### The keys are just secp256k1 keys

Nothing about them is Pocket-specific. A private key here is a **raw secp256k1
scalar: 32 bytes, hex-encoded — 64 hex characters**. That is the whole input.

```
2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a
```

No keyring, no armor, no mnemonic, no derivation path, no bech32, no
`pocketd`-specific encoding. If your language can hold 32 bytes and do secp256k1
arithmetic, you have everything the signing needs.

Everything else is derived from that scalar with standard operations:

| value | how |
|---|---|
| public key | `privkey * G`, serialized **compressed**: 33 bytes (`0x02`/`0x03` parity prefix + 32-byte X) |
| address | `bech32("pokt", ripemd160(sha256(pubkey33)))` — the standard Cosmos derivation |

Worked end to end, so you can check your own code against it:

```
private key  2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a
public key   0397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc5
address      pokt1mrqt5f7qh8uxs27cjm9t7v9e74a9vvdnq5jva4
```

Two traps, both real:

- **Do not hash the key.** Cosmos-SDK's `GenPrivKeyFromSecret` hashes its input
  to *derive* a key. These 32 bytes already *are* the key — feed them in
  directly. (Go: `&secp256k1.PrivKey{Key: bytes}`, not `GenPrivKeyFromSecret`.)
- **The address is not used in the signature.** It is only carried in the session
  header, where the relayer checks it matches the app's pinned/on-chain pubkey.
  The ring is built from **public keys**, never from addresses.

For a simulated relay these are keys you generate yourself; for a real relay the
signer's key belongs to a staked application, or to a gateway it delegated to.
**Same 32 bytes either way.**

### Real and simulated relays are signed the same way

**Signing a real relay and signing a simulated relay are the same act.** Same
algorithm, same bytes on the wire, same 32-byte keys. The only difference is
where the relayer looks up the ring it checks your signature against:

| | real relay | simulated relay |
|---|---|---|
| ring comes from | the chain: the app's own key + the gateways it delegated to | a ring pinned in the relayer's config |
| requires staking / chain access | yes | no |
| metered, claimed, paid | yes | never |
| private keys you need | **one** — the signer's | **one** — the signer's |
| **how you sign it** | **identical** | **identical** |

That is why the examples here use the simulated path: it lets you run the exact
signing code you would use in production, against a real relayer and a real
backend, without staking anything or talking to a chain. Get a simulated relay
accepted and you have a working relay signer — point it at a staked app and
gateway and it signs real ones.

See [`docs/simulated-relays.md`](../../docs/simulated-relays.md) for the
simulated path itself.

### A relay is not signed with a plain secp256k1 signature

It is signed with a **bLSAG ring signature** — the linkable ring signature from
the RingCT paper, over secp256k1. The signature proves *someone in the ring*
signed, without saying who, and publishes a key image that makes two signatures
by the same signer linkable.

The ring for a relay is `[application_pubkey, gateway_pubkey]` — two **public**
keys — and one of their private counterparts signs. Normally the gateway's. A
relayer that accepts the signature has learned only that *some* ring member
authorized the relay, which is precisely the guarantee delegation needs: the
gateway acts for the app, and never holds the app's key.

There is no specification document for this scheme. It is defined by what
[`pokt-network/ring-go`](https://github.com/pokt-network/ring-go) v0.2.0 and
`pokt-network/go-dleq` do. This README is a description of that behavior,
verified against their source and against a running verifier — not a standard
you can look up elsewhere.

### The algorithm

Curve secp256k1, group order
`N = fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141`.

Three different hashes are in play. Do not conflate them:

| purpose | hash |
|---|---|
| the message `m` being signed | SHA-256 of the marshaled `RelayRequest` with `Meta.Signature` set to nil |
| the challenge, `hashToScalar` | **SHA3-512** (FIPS-202), reduced mod N |
| hash-to-curve, `H_p` | **SHA3-256** (FIPS-202), try-and-increment |

All SHA3 here is **FIPS-202 SHA3, not Keccak-256**. Many libraries ship both,
often with confusingly similar names. Picking the wrong one produces a signature
that is silently and always rejected.

```
hashToScalar(in):
    n = int(SHA3-512(in)) mod N
    return quirk_encode(n)              # <-- see "The quirk" below

H_p(pubkey33):                          # try-and-increment
    h = SHA3-256(pubkey33)
    repeat up to 128 times:
        X = int(h)                      # as a secp256k1 FIELD element
        if a curve point exists at X with EVEN y: return it
        h = SHA3-256(h)                 # re-hash the DIGEST, not the pubkey

challenge(m, L, R) = hashToScalar(m || compress33(L) || compress33(R))

Sign(m, pubs[n], privKey x, ourIdx j):
    H_j = H_p(pubs[j])
    I   = x * H_j                       # key image
    u   = random scalar
    c[(j+1) % n] = challenge(m, u*G, u*H_j)
    for i in 1..n-1:
        idx    = (j+i) % n
        s[idx] = random scalar
        L      = s[idx]*G + c[idx]*pubs[idx]
        R      = s[idx]*H_p(pubs[idx]) + c[idx]*I
        c[(idx+1) % n] = challenge(m, L, R)
    s[j] = (u - c[j]*x) mod N           # closes the ring
```

Signing is **randomized**: `u` and every decoy `s[i]` come from a CSPRNG. Two
signatures over the same message differ. There is no deterministic nonce scheme,
so you cannot diff your signature against a reference one — you can only have it
verified. That is what the oracle is for.

Wire format, `69 + 65n` bytes (**199** for the 2-member ring):

```
[4B big-endian uint32 n][32B c[0]][33B key image I][ n × ( [32B s_i][33B pubs_i] ) ]
```

The ring's public keys travel **inside** the signature, interleaved with the `s`
values. The signature is self-contained.

### The quirk you must reproduce

`go-dleq`'s `hashToScalar` finishes like this
([`secp256k1/curve_decred.go:166-182`](https://github.com/pokt-network/go-dleq/blob/master/secp256k1/curve_decred.go)):

```go
n = SHA3-512(in) mod N
var reduced [32]byte
copy(reduced[:], n.Bytes())   // <-- here
```

`big.Int.Bytes()` returns the **minimal** big-endian encoding, and `copy` writes
from offset 0. So when `n` needs fewer than 32 bytes — i.e. whenever `n < 2^248`,
about **1 input in 256** — the value is **left-aligned**: the scalar becomes
`n << 8k`, not `n`.

Every reasonable implementation right-aligns (zero-pads on the left). **Your port
must not.** Reproduce the left-align, or your signatures are rejected whenever a
challenge lands on a short reduction:

```
P(rejected) = 1 - (255/256)^n     # n = ring size
            = 0.78%               # for the 2-member [app, gateway] ring
```

That is roughly **1 relay in 128** — and it is the worst failure rate there is.
It passes every round-trip test you write against your own verifier. It survives
code review, because the Go source looks correct. It only shows up against a real
relayer, as what looks like a flaky network.

This is measured, not predicted. `oracle_test.go` signs 4000 relays with a
canonical (right-aligning) implementation and watches ring-go reject them at the
predicted rate; writing the examples in this directory put roughly 6.2 million
samples through it, and the pooled rate matches 1/256. If `go-dleq` ever fixes
that line, that test fails — which is the alarm telling you every port here needs
the same one-line change, in lockstep with the fleet.

Be careful reading the rate off a single run: at 4000 signatures the count is a
random variable with a standard deviation of about 5.6, so anything from ~20 to
~45 rejections is unremarkable. Do not chase a 2-sigma run as if it were a bug.

Note the contrast that catches people: **scalars on the wire are canonical**
(32-byte big-endian, zero-padded on the left). Only the challenge derivation is
left-aligned. Same value, two encodings, one function apart.

**If you go read `go-dleq` to check this for yourself, read the build tag first.**
It ships two secp256k1 backends and they disagree:

| file | build tag | `HashToScalar` |
|---|---|---|
| `curve_decred.go` | `!ethereum_secp256k1` — **the default, what you run** | left-aligns (the quirk) |
| `curve_ethereum.go` | `cgo && ethereum_secp256k1` | canonical, **no quirk** |

Land in the wrong file, and you will "prove" the quirk does not exist. It is a
coin flip which one you open. The two backends are therefore **wire-incompatible
for ~1 challenge in 256**, which would make `-tags ethereum_secp256k1` a silent
consensus fork. This repository refuses to build with it
([`rings/ethereum_backend_unsupported.go`](../../rings/ethereum_backend_unsupported.go));
the scheme is defined by the default backend's behavior, quirk and all.

Two further consequences of how Go writes it, both easy to get subtly wrong:

- **Do not reduce mod N before the left-align.** Go left-aligns the *already
  reduced* value and then panics if the result comes back `>= N`
  (`wasReduced != 0` → `"hash should not be reduced twice"`). Reducing in the
  wrong order silently changes the scalar.
- **`hashToScalar` may return a value that is not reduced.** Go reduces at each
  *use site* instead. Reducing inside `hashToScalar` is equivalent only because
  every Go use site happens to reduce too — fine to rely on, worth knowing you
  are relying on it, and required in languages whose curve library rejects an
  out-of-range scalar outright.

### Ring order does not matter

You might expect to have to reproduce Go's ring ordering. You do not.

Both the relayer ([`rings/client.go`](../../rings/client.go) `ringPointsContain`)
and the chain (poktroll `pkg/crypto/rings/client.go`
`VerifyRelayRequestSignature`) check only that **every pubkey in your signature is
a member of the expected ring**. Neither compares order, and neither compares
size. The bech32-address sort you will find in the Go ring client only builds the
expected set; the set is then used as a map.

So `[app, gateway]` in that order, unsorted, is correct for both a simulated and
a real relay. Sign with the gateway key at index 1.

### Verifying your implementation: the oracle

A ring signature is correct only if the Go verifier guarding a real relayer
accepts it. Your own sign→verify round trip proves nothing: an implementation can
be perfectly self-consistent and rejected by every relayer for its whole life.

The oracle does not reimplement anything. It hands your bytes to the same
`ring-go` the relayer runs and reports the verdict.

```bash
go build -o /tmp/oracle ./examples/relay-signing/oracle/

/tmp/oracle vectors    # deterministic test vectors, to bisect a broken port
/tmp/oracle sign       # a known-good Go signature, for testing your verifier
echo '{"msg_hex":"..","sig_hex":".."}' | /tmp/oracle verify   # exit 0 = accepted
```

`vectors` publishes the intermediate values the signature is built from —
`hashToScalar`, `H_p`, scalar encoding — because when a port fails you need to
know *which primitive* is wrong, and the signature itself is randomized and
undiffable. It deliberately includes a **short reduction** vector: the case a
canonical implementation gets wrong.

**How to test properly.** The quirk fires on ~1 signature in 128, so a small test
suite passes for a **broken** implementation. Concretely, a 50-signature run
passes a quirk-less port `(1 - 0.0078)^50` ≈ **68%** of the time. The Node.js
example's first 50-signature smoke test passed on the first attempt — which told
it nothing at all. Every example here is therefore held to:

> sign **at least 1500** distinct messages, feed **every** signature to
> `oracle verify`, and require **100%** acceptance.

A port that does not reproduce the quirk fails roughly 12 of those 1500. The
suite size *is* the test's entire value; anything under ~1000 is theatre. If you
write your own signer, hold it to the same bar.

### The examples

| language | directory | dependencies | signatures accepted by `ring-go` |
|---|---|---|---|
| Node.js | [`nodejs/`](nodejs/) | `@noble/curves`, `@noble/hashes` | 100% |
| Python | [`python/`](python/) | none — standard library only | 100% |
| Rust | [`rust/`](rust/) | `k256`, `sha3`, `getrandom`, `thiserror`, `hex` | 100% |
| Go (reference) | [`oracle/`](oracle/) | the real `ring-go` | — |

Each one signs a relay and proves itself against the oracle: every signature it
produces is handed to the real Go verifier, and a rejection is a failure. Each
also runs the **negative control** — the same signer with a canonical
`hashToScalar` — because if that variant is *not* rejected, the main test is not
testing anything.

Start from the language you want. When a value disagrees, bisect it against
`oracle vectors` and read `oracle/primitives.go`.

Build the oracle once, then run whichever example you care about:

```bash
go build -o /tmp/oracle ./examples/relay-signing/oracle/

# Node.js
cd examples/relay-signing/nodejs && npm install && ORACLE=/tmp/oracle npm test

# Python  (no install step: standard library only)
cd examples/relay-signing/python && python3 verify_against_oracle.py --oracle /tmp/oracle

# Rust
cd examples/relay-signing/rust && cargo run --release -- /tmp/oracle
cargo run --release -- /tmp/oracle --negative-control   # prove the harness can fail
```

The examples are written to be read and copied. They are not published packages,
they are not constant-time, and they are not tuned for throughput — clarity was
chosen over all three, deliberately, because the point is to show how the scheme
works.

### Traps these examples actually fell into

Every one of these was hit while writing the code in this directory, and each
produces a signature that is silently rejected — no diagnostic, no exception,
just a relayer saying no.

**Any language**

- **A 65-byte uncompressed public key is silently wrong.** Most libraries accept
  compressed and uncompressed encodings as the same point, and `privkey*G == P`
  still passes — but `H_p` hashes the *raw bytes*, so the key image is wrong and
  **every** signature is rejected. Measured: compressed ring 20/20 accepted,
  uncompressed ring 0/20. Reject anything that is not 33 bytes.
- **Ring order is a convention, not a constraint.** `[app, gateway]` is what the
  Go client builds, but the relayer and chain only check set membership, so do
  not add a sort trying to match Go. Rings can be larger than two: an app may
  delegate to several gateways.

**Node.js**

- **`%` is truncated, not Euclidean.** `-1n % N === -1n`, while Go's `big.Int.Mod`
  is never negative. `s[j] = u - c*x` is negative about half the time, so a bare
  `%` rejects roughly **every second signature**. Use `((a % m) + m) % m`. From
  the outside this looks exactly like the quirk, at a wildly different rate.
- `@noble/curves` v2 renamed most of the v1 API that appears in online examples
  (`Point.fromBytes`/`toBytes`/`Point.Fn.ORDER`, not `ProjectivePoint`/
  `toRawBytes`/`CURVE.n`). Introspect the installed version.
- `Point.multiply()` throws on a scalar of `0` or `>= N`, which is exactly what
  the unreduced challenge can be — see the reduction note above.

**Python**

- Pure-integer affine arithmetic is fast enough and much clearer than Jacobian
  coordinates — but use `pow(a, -1, P)` (extended Euclid) rather than
  `pow(a, P-2, P)` (Fermat): it is ~20× faster and makes the difference.
- If you parallelize the test harness, pass state via `initializer`/`initargs`.
  A module global works under `fork` (Linux) and silently breaks under `spawn`
  (macOS, Windows).

**Rust**

- `sha3::Sha3_512` is FIPS-202; `sha3::Keccak512` is not. The crate ships both.
- `k256`'s `Scalar::from_repr` is a `CtOption` that rejects a non-canonical
  value, which the left-shifted challenge can be. Reduce instead:
  `<Scalar as Reduce<U256>>::reduce(&U256::from_be_bytes(bytes))`.
- **Randomness moved out of `rand_core`.** `k256` 0.14 pulls `elliptic-curve`
  0.14 → `rand_core` 0.10, which no longer has an `OsRng` at all. Take it from
  `getrandom` instead: `Scalar::random(&mut UnwrapErr(SysRng))`. The
  `UnwrapErr` wrapper is not decoration — `SysRng` is a `TryRngCore` and
  `Field::random` wants an infallible `RngCore`.
- `Reduce` only accepts `U256`, but SHA3-512 produces 64 bytes. Use
  `crypto-bigint`'s `U512 % NonZero(N)` (re-exported at
  `k256::elliptic_curve::bigint`) to mirror Go's `big.Int.Mod` directly.

### Sending the relay

Once signed, put the signature in `RelayRequest.Meta.Signature`, marshal, and
POST it:

```
POST {relayer}/{serviceID}
Content-Type: application/x-protobuf
Rpc-Type: 3                              # 1=gRPC 2=WebSocket 3=JSON-RPC 4=REST 5=CometBFT
Pocket-Simulation-Key-Id: <key_id>       # simulated relays only

body = marshal(RelayRequest{ payload, meta{ session_header, supplier_operator_address, signature } })
```

For a simulated relay the session header carries a synthetic session:
`session_id = simv1:<unixSeconds>:<hex(8 random bytes)>`, with
`session_start_block_height = 1` and `session_end_block_height = 2`. The
timestamp and nonce are inside the signed bytes, so they cannot be tampered with;
the relayer enforces a freshness window against its own clock and de-duplicates
signatures. Draw a fresh nonce per relay.

**Your protobuf encoder does not need to match Go byte-for-byte.** The relayer
unmarshals your request and re-marshals it internally to compute the signable
hash, so your bytes only need to be a *fixed point* of Go's unmarshal→marshal.
Encode fields in ascending field-number order, omit empty fields, and check it
once against a few lines of Go rather than reasoning about gogoproto.

`RelayRequest.Payload` is an opaque `bytes` field, hashed verbatim and parsed
separately by the relayer, so any protobuf library will do.

### Reading the response, and checking it

Signing is half the job. A relayer answers with a `RelayResponse`, and there are
two things in it worth verifying — plus one layer of wrapping that surprises
everyone the first time.

**The answer is two layers down.** `RelayResponse.Payload` is not your JSON-RPC
result: it is a marshalled `POKTHTTPResponse`, and the result is in its
`body_bz`. Status code and headers live there too.

**The supplier signature covers filtered bytes, not re-encoded ones.** What is
signed is the `RelayResponse` with the signature cleared and — when
`payload_hash` is set — the payload cleared. Get those bytes by *filtering the
protobuf you received*: walk its top-level records, drop field 2 inside field 1
(the signature), and — **only if field 4, `payload_hash`, is present** — drop
top-level field 2 as well. The condition is not decoration: responses from
before `payload_hash` existed sign the payload itself, and dropping it
unconditionally makes those fail. Do not decode and re-encode; a field's encoding does not
depend on the others and gogoproto emits in ascending field order, so filtering
is byte-exact while re-encoding risks differing on map ordering.

**Cosmos hashes twice, and this is the one that costs an evening.**
`secp256k1.Sign` and `VerifySignature` hash their argument internally. So the
message raw ECDSA covers is `sha256(sha256(signable_bytes))` for the response
and `sha256(digest)` for the receipt. Feed the digest straight to a raw verifier
and it rejects while Go insists the artifact is valid. The oracle publishes both
values — `response_ecdsa_message_hash_hex` at the top level and
`ecdsa_message_hash_hex` inside `receipt` — so you can tell which one you got
wrong.

**Low-S is not optional.** Cosmos rejects `S > N/2`. A verifier that accepts
high-S is more permissive than the chain, which is the one direction you must
never be wrong in.

#### Where the supplier's public key comes from

Every verifier here takes it as a parameter, and getting it from the wrong place
voids the whole exercise. It is **not** in the response: `RelayResponseMetadata`
carries a session header and a signature, nothing else. Taking it from a header,
or from the response body, verifies only that whoever wrote the response also
wrote the key.

You already know which supplier you sent to — it is the
`supplier_operator_address` you put in your own `RelayRequest`. Resolve that
address to a public key with a chain query: the account query returns the
operator account's public key, which is the key that signs both the response and
the receipt. That is what the CLI does
(`client/relay_client/client.go`, `SupplierPubKey`).

One failure mode worth handling: an account that has never signed a transaction
has no public key on chain yet, and the query comes back empty.

#### The receipt

Ask for one with `Pocket-Sign-Receipt: true` and the response carries
`Pocket-Relay-Receipt: v1.<128 hex chars>`:

```
digest  = sha256("POKT-RELAY-RECEIPT-v1\0" || relayRequest.Meta.Signature
                                            || relayResponse.PayloadHash)
receipt = supplierOperatorKey.Sign(digest)
```

It answers what the response signature cannot: *which request does this response
answer?* The response signature covers a session header that thousands of relays
share. Absence is normal — a relayer predating the feature returns no header, so
handle the missing case rather than treating it as an error.

#### Vectors and running it

```bash
go build -o /tmp/oracle ../oracle/

/tmp/oracle response-vectors                      # ground truth, JSON
/tmp/oracle response-vectors | python3 verify.py  # Python
/tmp/oracle response-vectors | node verify.mjs    # Node.js
cd ../rust && cargo run --release -- /tmp/oracle --response   # Rust

# have cosmos judge YOUR digest, not your own round trip
echo '{"digest_hex":"..","sig_hex":"..","pub_hex":".."}' | /tmp/oracle verify-receipt
```

The vectors ship negative controls — a different response's payload hash and a
different request's signature. Your verifier must **reject** both. A test that
only ever demonstrates success teaches nothing about what the receipt proves.


### Reference

- [`docs/simulated-relays.md`](../../docs/simulated-relays.md) — the simulated relay path, config, and CLI
- [`client/relay_client/simulated.go`](../../client/relay_client/simulated.go) — the Go implementation these examples mirror
- [`rings/pinned.go`](../../rings/pinned.go) — how the relayer verifies against a pinned ring
