#!/usr/bin/env python3
"""bLSAG ring signatures for Pocket Network relays — pure Python 3, zero dependencies.

A Pocket relay request is signed with a *ring* signature: the signature proves
"one of these keys signed it" without revealing which. For a relay the ring is
two members — the application and the gateway it delegated to — and the gateway
is the one holding a private key:

    ring    = [app_pubkey, gateway_pubkey]   # the conventional order
    our_idx = 1                              # the gateway signs

The relayer feeds your bytes to github.com/pokt-network/ring-go. If ring-go says
no, the relay is dropped. So this file is a translation of what that library
does, not an independent design. It is checked against the real verifier by
verify_against_oracle.py in this directory — run it after any edit.

../README.md explains the scheme, why a relay is signed this way, and how to put
the signature on the wire once you have it.

Quick check that this file works, from this directory:

    go build -o /tmp/oracle ../oracle/ && python3 sign.py | /tmp/oracle verify

Three different hash functions appear below. Mixing them up is the usual way
this goes wrong:

    message digest   SHA-256      (the 32-byte relay hash you pass in as `m`)
    hash_to_scalar   SHA3-512     FIPS-202 SHA3 — *not* Keccak-256
    hash_to_curve    SHA3-256     FIPS-202 SHA3 — *not* Keccak-256

Python's hashlib.sha3_* are FIPS-202, which is what you want here. The `pysha3`
and `pycryptodome` "keccak" functions are the *other* thing (Ethereum's
pre-standard Keccak) and will silently produce signatures that never verify.
Nothing else in this file needs a dependency, so don't add one.

Two details in here are surprising enough that they are called out where they
happen. If you only read two comments, read these:

  * hash_to_scalar() left-aligns a short reduction. This is a quirk of the Go
    implementation, not a design choice, and it fires about 1 signature in 128.
  * encode_scalar() does NOT. Wire scalars are canonical. The contrast is real.

Not constant-time, and it makes no attempt to be: the scalar multiply below
branches on secret bits and Python ints are not fixed-width. It is written to
be read. For production signing, use a vetted library — or accept that this
process must not share a machine with anything you don't trust.
"""

from __future__ import annotations

import hashlib
import secrets
from typing import List, Optional, Sequence, Tuple

# --------------------------------------------------------------------------
# secp256k1
# --------------------------------------------------------------------------

# Field prime. y^2 = x^3 + 7 over F_P.
P = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEFFFFFC2F
# Order of the generator: scalars live mod N.
N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141
# Curve coefficients: a = 0 is why point_double() below has no `+ a` term.
A = 0
B = 7
# Generator point G.
Gx = 0x79BE667EF9DCBBAC55A06295CE870B07029BFCDB2DCE28D959F2815B16F81798
Gy = 0x483ADA7726A3C4655DA4FBFC0E1108A8FD17B448A68554199C47D08FFB10D4B8

# A point is (x, y), or None for the point at infinity (the group identity).
Point = Optional[Tuple[int, int]]

G: Point = (Gx, Gy)

# Ring size is serialized as 4 bytes, and each member costs 65 bytes on the wire.
SIGNATURE_HEADER_SIZE = 69  # 4 (ring size) + 32 (c[0]) + 33 (key image)
SIGNATURE_MEMBER_SIZE = 65  # 32 (s_i) + 33 (P_i)


# --------------------------------------------------------------------------
# Point arithmetic
#
# Textbook affine formulas. Every addition pays for a modular inverse, which
# real libraries avoid with Jacobian coordinates. Using pow(a, -1, P) — Python's
# extended-Euclid inverse, ~3 us — instead of a Fermat exponentiation (~66 us)
# keeps a whole signature near 30 ms on a laptop. Plenty for signing relays, and
# far easier to check by eye than the Jacobian version would be.
# --------------------------------------------------------------------------


def point_add(p: Point, q: Point) -> Point:
    """Add two points on secp256k1."""
    if p is None:
        return q
    if q is None:
        return p

    x1, y1 = p
    x2, y2 = q

    if x1 == x2:
        # Same x: either q == -p (vertical line, sums to infinity) or q == p.
        if (y1 + y2) % P == 0:
            return None
        return point_double(p)

    lam = (y2 - y1) * pow(x2 - x1, -1, P) % P
    x3 = (lam * lam - x1 - x2) % P
    y3 = (lam * (x1 - x3) - y1) % P
    return (x3, y3)


def point_double(p: Point) -> Point:
    """Add a point to itself."""
    if p is None:
        return None

    x, y = p
    if y == 0:
        return None

    # lam = (3x^2 + a) / 2y, and a == 0 on secp256k1.
    lam = (3 * x * x) * pow(2 * y, -1, P) % P
    x3 = (lam * lam - 2 * x) % P
    y3 = (lam * (x - x3) - y) % P
    return (x3, y3)


def scalar_mult(k: int, p: Point) -> Point:
    """Multiply point `p` by scalar `k` (double-and-add).

    `k` is reduced mod N first. That matters: hash_to_scalar() can hand back a
    value that is not reduced, and Go reduces at exactly this boundary too.
    """
    k %= N
    if k == 0 or p is None:
        return None

    result: Point = None
    addend: Point = p
    while k:
        if k & 1:
            result = point_add(result, addend)
        addend = point_double(addend)
        k >>= 1
    return result


def scalar_base_mult(k: int) -> Point:
    """Multiply the generator G by scalar `k`."""
    return scalar_mult(k, G)


def _decompress_y(x: int, want_odd: bool) -> Optional[int]:
    """Return the y of the requested parity with (x, y) on the curve, else None.

    About half of all x values have no y at all — that is not an error, it is
    how try-and-increment in hash_to_curve() makes progress.

    Mirrors decred's secp256k1.DecompressY.
    """
    alpha = (pow(x, 3, P) + B) % P

    # P % 4 == 3, so if alpha has a square root at all, it is alpha^((P+1)/4).
    y = pow(alpha, (P + 1) // 4, P)
    if (y * y) % P != alpha:
        return None  # alpha is not a quadratic residue: no point with this x

    if bool(y & 1) != want_odd:
        y = (-y) % P
    return y


# --------------------------------------------------------------------------
# Encoding
# --------------------------------------------------------------------------


def compress_point(p: Point) -> bytes:
    """Serialize a point as 33-byte compressed SEC1: 0x02/0x03 then x."""
    if p is None:
        # Unreachable with honest inputs, and there is no encoding for it.
        raise ValueError("cannot compress the point at infinity")
    x, y = p
    return bytes([0x02 + (y & 1)]) + x.to_bytes(32, "big")


def decompress_point(data: bytes) -> Point:
    """Parse a 33-byte compressed SEC1 point, checking it is really on the curve."""
    if len(data) != 33:
        raise ValueError(f"compressed point must be 33 bytes, got {len(data)}")
    if data[0] not in (0x02, 0x03):
        raise ValueError(
            f"compressed point must start with 0x02 or 0x03, got {data[0]:#04x} "
            "(0x04 is an uncompressed key — the ring wants compressed)"
        )

    x = int.from_bytes(data[1:], "big")
    if x >= P:
        raise ValueError("x coordinate is not a field element")

    # 0x02 => even y, 0x03 => odd y.
    y = _decompress_y(x, want_odd=bool(data[0] & 1))
    if y is None:
        raise ValueError("point is not on the secp256k1 curve")
    return (x, y)


def encode_scalar(n: int) -> bytes:
    """Encode a scalar for the wire: 32 bytes big-endian, zero-padded on the LEFT.

    This is the canonical, boring encoding you would expect — and it is worth
    noticing that hash_to_scalar() below does the opposite. Both are correct;
    they are different steps. Wire scalars are canonical. Only the challenge
    derivation left-aligns.

    Mirrors the oracle's encodeScalar.
    """
    return (n % N).to_bytes(32, "big")


def public_key_from_private(priv_key: bytes) -> bytes:
    """Return the 33-byte compressed public key for a 32-byte private key."""
    return compress_point(scalar_base_mult(_scalar_from_private(priv_key)))


def _scalar_from_private(priv_key: bytes) -> int:
    """Validate a private key and return it as a scalar."""
    if isinstance(priv_key, int):
        x = priv_key
    else:
        if len(priv_key) != 32:
            raise ValueError(f"private key must be 32 bytes, got {len(priv_key)}")
        x = int.from_bytes(priv_key, "big")
    if not 1 <= x < N:
        raise ValueError("private key out of range [1, N)")
    return x


def _random_scalar() -> int:
    """A uniform scalar in [1, N), from the OS CSPRNG.

    Every signature needs fresh randomness here. Reusing `u` across two
    signatures with the same key leaks the private key outright, so this must
    never be seeded, cached, or made deterministic "for testing".
    """
    return 1 + secrets.randbelow(N - 1)


# --------------------------------------------------------------------------
# The two Pocket-specific primitives
# --------------------------------------------------------------------------


def hash_to_scalar(data: bytes) -> int:
    """Hash to a scalar: SHA3-512, reduce mod N, then re-encode Go's way.

    That last step is where ports break, so it is worth being precise about it.
    go-dleq (secp256k1/curve_decred.go, HashToScalar) does:

        n := new(big.Int).Mod(n, c.order)
        var reduced [32]byte
        copy(reduced[:], n.Bytes())        // <-- here

    big.Int.Bytes() returns the *minimal* big-endian encoding — 31 bytes for a
    value below 2^248 — and copy() writes it at offset 0. So a short value lands
    LEFT-aligned in the 32-byte buffer, padded with zeros on the right, and
    reads back as n << 8k when it was k bytes short: n * 256 in the common case.
    Not n. The Go call that would zero-pad on the left is FillBytes, and it is
    not the one that was written.

    A reduction is short whenever it lands below 2^248: about 1 input in 256.
    Right-aligning instead (the natural choice, and what every from-scratch port
    writes on the first try) agrees with Go on the other 255, which is exactly
    what makes this expensive to find. A 2-member ring computes 2 challenges, so
    ~1 relay in 128 gets rejected: too rare to reproduce on demand, common
    enough to bleed relays all day.

    So: this is a bug-compatible port. The quirk is load-bearing, and the
    oracle's TestCanonicalHashToScalarIsRejected exists to prove it — it fails
    if anyone "cleans this up".

    Returns the scalar as an int, which callers reduce mod N at the point of
    use, exactly as the Go does.
    """
    h = hashlib.sha3_512(data).digest()  # FIPS-202 SHA3-512, NOT Keccak-512
    n = int.from_bytes(h, "big") % N

    # Go's big.Int.Bytes(): minimal big-endian, and empty for zero.
    minimal = n.to_bytes((n.bit_length() + 7) // 8, "big")

    # Go's copy(reduced[:], ...): writes at offset 0 => left-aligned.
    reduced = bytearray(32)
    reduced[: len(minimal)] = minimal

    return int.from_bytes(reduced, "big")


def hash_to_curve(pubkey33: bytes) -> Point:
    """Hash a compressed public key to a curve point (try-and-increment).

    SHA3-256 the key, read the digest as an x coordinate, and take the point
    with EVEN y. Roughly half of all x values are not on the curve; on a miss,
    re-hash *the digest* — not the public key — and try again.

    Mirrors ring-go's hashToCurveSecp256k1 (helpers.go), including its 128-try
    safety bound.
    """
    digest = hashlib.sha3_256(pubkey33).digest()  # FIPS-202 SHA3-256

    for _ in range(128):
        # Go reads the digest into a FieldVal and normalizes, which is a
        # reduction mod P. Unreachable in practice (P is within 2^32 of 2^256)
        # but this is what it means.
        x = int.from_bytes(digest, "big") % P

        y = _decompress_y(x, want_odd=False)  # ring-go passes odd=false
        if y is not None:
            return (x, y)

        digest = hashlib.sha3_256(digest).digest()  # re-hash THE DIGEST

    # 128 consecutive misses has probability 2^-128.
    raise ValueError("hash_to_curve found no point in 128 attempts")


def challenge(m: bytes, L: Point, R: Point) -> int:
    """c = hash_to_scalar(m || L || R), with both points compressed.

    Upper case to match the scheme's notation, and because a lone lowercase `l`
    is impossible to tell from a `1` in most fonts.
    """
    return hash_to_scalar(m + compress_point(L) + compress_point(R))


# --------------------------------------------------------------------------
# Signing
# --------------------------------------------------------------------------


def build_relay_signature(
    m: bytes,
    ring_pubkeys: Sequence[bytes],
    priv_key: bytes,
    our_idx: int,
) -> bytes:
    """Sign a 32-byte relay hash with a bLSAG ring signature.

    Args:
        m: the 32-byte message. For a relay this is GetSignableBytesHash():
           SHA-256 of the proto-marshaled RelayRequest with Meta.Signature
           cleared to nil first. Exactly 32 bytes.
        ring_pubkeys: the ring, as 33-byte compressed public keys. For a relay
           this is [app_pubkey, gateway_pubkey]. The keys travel inside the
           signature, and the relayer checks only that each one belongs to the
           ring it expects — not their order, and not how many (see
           ringPointsContain in rings/client.go). So this order is a
           convention, not a constraint: whatever order you pass is what gets
           signed, and any order verifies. Don't go looking for Go's bech32
           sort — that only builds the expected set, which is then used as a map.
        priv_key: the 32-byte private key of ring_pubkeys[our_idx]. For a relay
           this is the gateway's key.
        our_idx: which ring member is signing. For a relay: 1, the gateway.

    Returns:
        The serialized signature: 69 + 65n bytes, so 199 for a 2-member ring.

    Raises:
        ValueError: on malformed inputs, including the easy-to-miss case of
            our_idx pointing at a key you don't hold.

    The scheme: build a loop of challenges around the ring, where every member
    except you gets a random s_i and a challenge derived from it, and you close
    the loop at the end with the one value only your private key can produce.
    A verifier walks the loop and checks it comes back to where it started; it
    cannot tell which link was the one that got to cheat.
    """
    if len(m) != 32:
        raise ValueError(f"message must be exactly 32 bytes, got {len(m)}")

    size = len(ring_pubkeys)
    if size < 2:
        raise ValueError(f"ring needs at least 2 members, got {size}")
    if not 0 <= our_idx < size:
        raise ValueError(f"our_idx {our_idx} out of range for a {size}-member ring")

    x = _scalar_from_private(priv_key)

    # Parse every key. This also re-serializes them canonically below, so a
    # non-canonical input can't produce a signature the relayer would reject
    # for a reason that has nothing to do with the maths.
    pubs: List[Point] = [decompress_point(pk) for pk in ring_pubkeys]
    pubs_bz: List[bytes] = [compress_point(pt) for pt in pubs]

    # ring-go's Sign() refuses if the key at our_idx isn't ours, and catching it
    # here is much kinder than a rejected relay. For a relay the gateway is
    # index 1; passing 0 is the mistake this catches.
    if pubs[our_idx] != scalar_base_mult(x):
        raise ValueError(
            f"ring_pubkeys[{our_idx}] is not the public key for priv_key "
            "(for a relay the gateway signs, so our_idx should be 1)"
        )

    c: List[int] = [0] * size
    s: List[int] = [0] * size

    # H_j = hash_to_curve(our key), and the key image I = x * H_j. The image is
    # deterministic in the key, which is what lets a verifier spot the same
    # signer twice (double-spend protection) without learning who it is.
    h_our = hash_to_curve(pubs_bz[our_idx])
    image = scalar_mult(x, h_our)

    # Start the loop at our own link with a random u, and hand the resulting
    # challenge to the next member.
    u = _random_scalar()
    c[(our_idx + 1) % size] = challenge(m, scalar_base_mult(u), scalar_mult(u, h_our))

    # Walk the rest of the ring. Each member gets a random s_i, and its
    # challenge follows from the previous one, all the way back around to us.
    for i in range(1, size):
        idx = (our_idx + i) % size
        s[idx] = _random_scalar()

        # L_i = s_i*G + c_i*P_i
        L = point_add(scalar_mult(c[idx], pubs[idx]), scalar_base_mult(s[idx]))
        # R_i = s_i*H(P_i) + c_i*I
        R = point_add(
            scalar_mult(c[idx], image),
            scalar_mult(s[idx], hash_to_curve(pubs_bz[idx])),
        )
        c[(idx + 1) % size] = challenge(m, L, R)

    # Close the loop: pick s_j so that s_j*G + c_j*P_j == u*G, which needs x.
    s[our_idx] = (u - c[our_idx] * x) % N

    # [4B ring size][32B c[0]][33B key image][n x (32B s_i || 33B P_i)]
    out = bytearray()
    out += size.to_bytes(4, "big")
    out += encode_scalar(c[0])
    out += compress_point(image)
    for i in range(size):
        out += encode_scalar(s[i])
        out += pubs_bz[i]

    assert len(out) == SIGNATURE_HEADER_SIZE + SIGNATURE_MEMBER_SIZE * size
    return bytes(out)


# --------------------------------------------------------------------------
# Verifying
#
# A gateway only ever signs — the relayer is what verifies. This is here
# because it is the clearest statement of what the signature above actually
# claims, and because it lets verify_against_oracle.py test the other
# direction: that this code accepts a signature produced by real Go.
# --------------------------------------------------------------------------


def verify_relay_signature(m: bytes, sig: bytes) -> bool:
    """Check a serialized ring signature. Returns True if the challenge loop closes.

    Any `sig` at all is safe to pass: bad bytes return False rather than raising,
    because a signature arrives from the network and being malformed is an
    expected answer, not an exception. A wrong-length `m` still raises — that one
    is your bug, not the network's.
    """
    if len(m) != 32:
        raise ValueError(f"message must be exactly 32 bytes, got {len(m)}")
    if len(sig) < SIGNATURE_HEADER_SIZE:
        return False

    size = int.from_bytes(sig[0:4], "big")
    if size < 2 or len(sig) != SIGNATURE_HEADER_SIZE + SIGNATURE_MEMBER_SIZE * size:
        return False

    try:
        c0 = int.from_bytes(sig[4:36], "big")
        image = decompress_point(sig[36:69])

        s: List[int] = []
        pubs: List[Point] = []
        pubs_bz: List[bytes] = []
        for i in range(size):
            off = SIGNATURE_HEADER_SIZE + SIGNATURE_MEMBER_SIZE * i
            s.append(int.from_bytes(sig[off : off + 32], "big"))
            pubs_bz.append(sig[off + 32 : off + 65])
            pubs.append(decompress_point(pubs_bz[i]))
    except ValueError:
        return False

    # Walk the loop: each challenge determines the next, and the last one must
    # land back on c[0]. Only someone who knew a private key could have made
    # that happen.
    c = c0
    for i in range(size):
        L = point_add(scalar_mult(c, pubs[i]), scalar_base_mult(s[i]))
        R = point_add(scalar_mult(c, image), scalar_mult(s[i], hash_to_curve(pubs_bz[i])))

        # Crafted input can steer L or R onto the point at infinity — put P_i = G
        # and s_i = -c mod N and L_i vanishes. An honest signature never does
        # (our own link is L_j = u*G with u in [1, N)), so this only ever means
        # the signature is junk. ring-go encodes the identity as 02||00..00,
        # hashes it, and the chain then cannot close, so it rejects; reject too.
        # Without this, compress_point() would raise out of a function whose
        # whole job is to answer yes or no.
        if L is None or R is None:
            return False

        c = challenge(m, L, R)

    return c % N == c0 % N


# --------------------------------------------------------------------------
# Demo: `python3 sign.py | oracle verify` should print {"valid":true}
# --------------------------------------------------------------------------

if __name__ == "__main__":
    import json

    # The oracle's throwaway test keys (examples/relay-signing/oracle/main.go).
    APP_PRIV = bytes.fromhex("c188c43496351a963762a5d9de78ff887ac66b4ba5de5967efd55a6d1e71ddda")
    GW_PRIV = bytes.fromhex("1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11")

    # ring = [app, gateway]; the gateway signs, so our_idx = 1.
    ring = [public_key_from_private(APP_PRIV), public_key_from_private(GW_PRIV)]
    msg = b"pocket-relay-signable-hash-32byt"  # stand-in for the real 32-byte hash

    signature = build_relay_signature(msg, ring, GW_PRIV, our_idx=1)

    print(json.dumps({"msg_hex": msg.hex(), "sig_hex": signature.hex()}))
