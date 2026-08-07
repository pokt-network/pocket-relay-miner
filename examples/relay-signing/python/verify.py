#!/usr/bin/env python3
"""Check what a Pocket relayer sends back — pure Python 3, zero dependencies.

sign.py in this directory covers the outbound half: how a relay request is
signed. This file covers the return trip, which is the half that decides whether
you can believe the answer:

    1. the supplier's signature over the RelayResponse   -- who produced this
    2. the payload hash binding                          -- ...and this body
    3. the optional Pocket-Relay-Receipt header          -- ...to WHICH request
    4. the JSON-RPC answer inside the response payload   -- what you asked for

That is the order, and it is not cosmetic: nothing you read out of the payload
in step 4 is worth anything until steps 1 to 3 have said whose payload it is.

Step 2 is not optional garnish. The signature does NOT cover the payload: it
covers a SHA-256 of it (field 4, `payload_hash`), and the payload travels
alongside. Verify the signature without re-hashing the payload and anyone on the
path can swap the body for another one and your check still says "valid".

Step 3 answers a question steps 1 and 2 cannot. A session header is shared by
thousands of relays, so a valid response signature only says "this supplier
produced this body somewhere in this session" — it does not say the body answers
YOUR request. The receipt binds the request's ring signature to this response's
payload hash, and only the supplier's key can produce it.

THE ENTRY POINT IS open_relay_response(). It runs all four steps in that order
and hands back the backend's answer, and it is the same function, under the same
name and with the same scope, in the Node and Rust ports next door. Everything
else here is a piece of it, exported so a port can bisect against the oracle one
layer at a time — and a name ending in `_only` is a piece, not a verification.

Nothing here needs a library. Protobuf is a few tag/length records — see
iter_fields() — and ECDSA over secp256k1 is twenty lines once you have the curve
arithmetic, which sign.py already has and this file imports rather than repeats.
That is the point: there is no magic in the wire format, and a port to any
language does not start with a dependency hunt.

Quick check that this file works, from this directory:

    go build -o /tmp/oracle ../oracle/ && /tmp/oracle response-vectors | python3 verify.py

For the pass/fail harness — including the negative controls and a cross-check
against the Go library the relayer actually runs — see verify_against_oracle.py.

Two traps are worth knowing before you read further, because each one produces a
verifier that is wrong in a way nothing in the error message will mention:

  * THE DOUBLE HASH. cosmos `secp256k1` hashes its input again, internally, on
    both sign and verify. The relayer hands it a 32-byte SHA-256 digest, so the
    hash the ECDSA maths actually runs on is sha256(sha256(...)). Hash once and
    every signature looks invalid. See verify_cosmos_signature().
  * LOW-S IS MANDATORY. Cosmos rejects any signature with S > N/2. So must you.
    Of all the ways to get a verifier wrong, being more permissive than the
    chain is the only one that cannot be caught by testing against valid inputs.

Not constant-time, and it does not need to be: everything here is public data.
"""

from __future__ import annotations

import hashlib
from typing import Dict, Iterator, List, NamedTuple, Optional, Tuple

# The curve lives in sign.py. Same file, same arithmetic — importing it keeps
# there from being two copies of secp256k1 in one directory that could drift.
from sign import N, decompress_point, point_add, scalar_base_mult, scalar_mult

# --------------------------------------------------------------------------
# Field numbers
#
# Restated from the .proto rather than generated, because generating them is
# what a reader would have to install a toolchain for. Sources:
#
#   RelayResponse, RelayResponseMetadata, RelayMinerError
#       poktroll/proto/pocket/service/relay.proto
#   SessionHeader
#       poktroll/proto/pocket/session/types.proto
#   POKTHTTPResponse, Header
#       shannon-sdk/proto/types/http.proto
#
# Field numbers are the wire contract: a rename is free, a renumber is not.
# --------------------------------------------------------------------------

# RelayResponse
RESP_META = 1
RESP_PAYLOAD = 2
RESP_RELAY_MINER_ERROR = 3
RESP_PAYLOAD_HASH = 4

# RelayResponseMetadata
META_SESSION_HEADER = 1
META_SUPPLIER_OPERATOR_SIGNATURE = 2

# SessionHeader
HDR_APPLICATION_ADDRESS = 1
HDR_SERVICE_ID = 2
HDR_SESSION_ID = 3
HDR_SESSION_START_BLOCK_HEIGHT = 4
HDR_SESSION_END_BLOCK_HEIGHT = 5

# RelayMinerError
ERR_CODESPACE = 1
ERR_CODE = 2
ERR_DESCRIPTION = 3

# POKTHTTPResponse
HTTP_STATUS_CODE = 1
HTTP_HEADER = 2
HTTP_BODY_BZ = 3

# Header, and the synthetic message protobuf generates for one map entry.
# Header.key is field 1 and repeats the map key; _parse_header_entry() reads
# the map key instead, so there is no constant for it.
HEADER_VALUES = 2
MAP_ENTRY_KEY = 1
MAP_ENTRY_VALUE = 2

# The supplier operator signature is 64 bytes, R || S, and the public key is a
# 33-byte compressed secp256k1 point.
SIGNATURE_SIZE = 64
PUBKEY_SIZE = 33
SHA256_SIZE = 32


# --------------------------------------------------------------------------
# Protobuf wire format, only as much as is needed
#
# A protobuf message is a flat sequence of records. Each one starts with a
# varint tag holding a field number and a wire type; what follows depends on the
# wire type. Unknown fields are skippable by construction, which is the whole
# reason this file can read a message it has no schema for.
# --------------------------------------------------------------------------

WIRE_VARINT = 0  # int32/64, uint32/64, bool, enum
WIRE_FIXED64 = 1  # fixed64, sfixed64, double
WIRE_LEN = 2  # string, bytes, embedded messages, packed repeated
WIRE_FIXED32 = 5  # fixed32, sfixed32, float


class Field(NamedTuple):
    """One record, as it appeared on the wire."""

    number: int
    wire_type: int
    #: Payload of a WIRE_LEN field, or the raw bytes of a fixed-width one.
    #: Empty for varints, whose value is in `varint`.
    data: bytes
    #: Decoded value of a WIRE_VARINT field. Zero for every other wire type.
    varint: int
    #: Tag, length and payload, byte for byte as received. Concatenating the
    #: `raw` of a subset of fields is how signable bytes get built below.
    raw: bytes


def read_varint(buf: bytes, pos: int) -> Tuple[int, int]:
    """Read a base-128 varint at `pos`. Returns (value, position after it)."""
    result = 0
    shift = 0
    while True:
        if pos >= len(buf):
            raise ValueError("truncated varint: ran off the end of the buffer")
        if shift > 63:
            raise ValueError("varint is longer than 64 bits")
        byte = buf[pos]
        pos += 1
        result |= (byte & 0x7F) << shift
        if not byte & 0x80:
            return result, pos
        shift += 7


def write_varint(value: int) -> bytes:
    """Encode a non-negative int as a varint, in the minimal number of bytes."""
    if value < 0:
        raise ValueError("cannot encode a negative value as an unsigned varint")
    out = bytearray()
    while True:
        byte = value & 0x7F
        value >>= 7
        if value:
            out.append(byte | 0x80)
        else:
            out.append(byte)
            return bytes(out)


def iter_fields(buf: bytes) -> Iterator[Field]:
    """Walk a protobuf message, yielding one Field per record, in wire order.

    Repeated fields appear once per occurrence, and so does a field a sloppy
    encoder emitted twice — this does not deduplicate, because deduplicating
    would mean deciding a policy, and the callers below each want their own.

    Raises ValueError on anything malformed, including the deprecated group
    wire types (3 and 4), which nothing in Pocket's protos uses.
    """
    pos = 0
    end = len(buf)
    while pos < end:
        start = pos
        tag, pos = read_varint(buf, pos)
        number = tag >> 3
        wire_type = tag & 0x07
        if number == 0:
            raise ValueError("field number 0 is not valid")

        if wire_type == WIRE_VARINT:
            value, pos = read_varint(buf, pos)
            yield Field(number, wire_type, b"", value, buf[start:pos])
        elif wire_type == WIRE_LEN:
            length, pos = read_varint(buf, pos)
            if length > end - pos:
                raise ValueError(
                    f"field {number} claims {length} bytes but only {end - pos} remain"
                )
            data = buf[pos : pos + length]
            pos += length
            yield Field(number, wire_type, data, 0, buf[start:pos])
        elif wire_type in (WIRE_FIXED64, WIRE_FIXED32):
            width = 8 if wire_type == WIRE_FIXED64 else 4
            if width > end - pos:
                raise ValueError(f"field {number} is a truncated fixed{width * 8}")
            data = buf[pos : pos + width]
            pos += width
            yield Field(number, wire_type, data, 0, buf[start:pos])
        else:
            raise ValueError(f"field {number} uses unsupported wire type {wire_type}")


def _first(buf: bytes, number: int) -> Optional[Field]:
    """The first record with this field number, or None.

    Proto3 resolves a duplicated scalar to the LAST occurrence, so reading the
    first could in principle disagree with the relayer. It cannot matter here,
    and the reason is worth understanding because it is the payoff for
    filtering rather than re-encoding: filter_signable_bytes() copies every
    record it keeps, duplicates included, so a second copy of any field is
    inside the hashed bytes. The supplier never signed those bytes, so such a
    message fails the signature check outright and never reaches a reader that
    could be confused about which copy counts.
    """
    for field in iter_fields(buf):
        if field.number == number:
            return field
    return None


def _string(field: Optional[Field]) -> str:
    """Decode a proto3 string field, which is required to be valid UTF-8."""
    return field.data.decode("utf-8") if field is not None else ""


def _bytes(field: Optional[Field]) -> bytes:
    return field.data if field is not None else b""


# --------------------------------------------------------------------------
# The RelayResponse
# --------------------------------------------------------------------------


class SessionHeader(NamedTuple):
    """Which application, service and session this relay belonged to."""

    application_address: str
    service_id: str
    session_id: str
    session_start_block_height: int
    session_end_block_height: int


class RelayMinerError(NamedTuple):
    """Set when the relayer itself failed, rather than the backend service.

    A response carrying one of these is still signed, and still genuine. It is
    a signed statement that the relay did not happen — check for it before
    trying to read a JSON-RPC answer out of the payload.
    """

    codespace: str
    code: int
    description: str


class RelayResponse(NamedTuple):
    """A parsed RelayResponse. `raw` is kept because the signature covers bytes."""

    session_header: Optional[SessionHeader]
    supplier_operator_signature: bytes
    payload: bytes
    payload_hash: bytes
    relay_miner_error: Optional[RelayMinerError]
    raw: bytes


def parse_session_header(buf: bytes) -> SessionHeader:
    """Parse a SessionHeader message."""
    fields = {f.number: f for f in iter_fields(buf)}
    start = fields.get(HDR_SESSION_START_BLOCK_HEIGHT)
    end = fields.get(HDR_SESSION_END_BLOCK_HEIGHT)
    return SessionHeader(
        application_address=_string(fields.get(HDR_APPLICATION_ADDRESS)),
        service_id=_string(fields.get(HDR_SERVICE_ID)),
        session_id=_string(fields.get(HDR_SESSION_ID)),
        session_start_block_height=start.varint if start else 0,
        session_end_block_height=end.varint if end else 0,
    )


def parse_relay_miner_error(buf: bytes) -> RelayMinerError:
    """Parse a RelayMinerError message."""
    fields = {f.number: f for f in iter_fields(buf)}
    code = fields.get(ERR_CODE)
    return RelayMinerError(
        codespace=_string(fields.get(ERR_CODESPACE)),
        code=code.varint if code else 0,
        description=_string(fields.get(ERR_DESCRIPTION)),
    )


def parse_relay_response(relay_response_bz: bytes) -> RelayResponse:
    """Parse the RelayResponse a relayer returned.

    Args:
        relay_response_bz: the marshalled RelayResponse, exactly as received.

    Raises:
        ValueError: if the bytes are not a well-formed protobuf message.
    """
    meta = _first(relay_response_bz, RESP_META)
    session_header = None
    signature = b""
    if meta is not None:
        header_field = _first(meta.data, META_SESSION_HEADER)
        if header_field is not None:
            session_header = parse_session_header(header_field.data)
        signature = _bytes(_first(meta.data, META_SUPPLIER_OPERATOR_SIGNATURE))

    error_field = _first(relay_response_bz, RESP_RELAY_MINER_ERROR)

    return RelayResponse(
        session_header=session_header,
        supplier_operator_signature=signature,
        payload=_bytes(_first(relay_response_bz, RESP_PAYLOAD)),
        payload_hash=_bytes(_first(relay_response_bz, RESP_PAYLOAD_HASH)),
        relay_miner_error=(
            parse_relay_miner_error(error_field.data) if error_field is not None else None
        ),
        raw=relay_response_bz,
    )


# --------------------------------------------------------------------------
# Signable bytes
# --------------------------------------------------------------------------


def filter_signable_bytes(relay_response_bz: bytes) -> bytes:
    """Return the bytes the supplier's signature covers.

    Two things come out, and nothing else changes:

        * RelayResponseMetadata.supplier_operator_signature (field 2 of field 1)
          -- a signature cannot cover itself.
        * RelayResponse.payload (field 2) -- but ONLY when payload_hash (field 4)
          is set. It stands in for the payload so that an onchain proof does not
          have to carry the whole body. See the conditional in poktroll's
          RelayResponse.GetSignableBytesHash: relayers older than v0.1.25 do not
          set payload_hash, and for those the payload IS signed. Dropping it
          unconditionally makes every pre-v0.1.25 response look forged.

    FILTER, DO NOT RE-ENCODE. It is tempting to parse the message into structs,
    clear two fields and marshal it again. Don't: your encoder has to agree with
    gogoproto on map entry ordering, on whether a zero-valued field is emitted,
    and on varint minimality, and a single disagreement changes the hash and
    rejects a perfectly good response. Copying the records you keep, byte for
    byte, cannot disagree with anything. This works because protobuf records are
    independent — no record's encoding depends on another's — and gogoproto
    emits them in ascending field order, so dropping some leaves the rest in a
    valid message with the ordering the signer used.

    The one byte sequence not copied verbatim is the metadata's length prefix,
    which has to shrink by the size of the signature that came out of it. That
    is arithmetic on a length, not a re-encoding of content.

    Args:
        relay_response_bz: the marshalled RelayResponse, exactly as received.

    Returns:
        The filtered message. SHA-256 it to get what the supplier key signed —
        or call response_signable_hash(), which does both.
    """
    drop_payload = bool(_bytes(_first(relay_response_bz, RESP_PAYLOAD_HASH)))

    out = bytearray()
    for field in iter_fields(relay_response_bz):
        if field.number == RESP_PAYLOAD and drop_payload:
            continue

        if field.number == RESP_META and field.wire_type == WIRE_LEN:
            inner = b"".join(
                sub.raw
                for sub in iter_fields(field.data)
                if sub.number != META_SUPPLIER_OPERATOR_SIGNATURE
            )
            out += write_varint(RESP_META << 3 | WIRE_LEN)
            out += write_varint(len(inner))
            out += inner
            continue

        out += field.raw

    return bytes(out)


def response_signable_hash(relay_response_bz: bytes) -> bytes:
    """SHA-256 of the filtered signable bytes: the 32 bytes handed to the key.

    Hashing first is not decoration. It is what makes the signed input a
    constant 32 bytes no matter how large the response was.
    """
    return hashlib.sha256(filter_signable_bytes(relay_response_bz)).digest()


# --------------------------------------------------------------------------
# secp256k1 ECDSA, the way cosmos does it
# --------------------------------------------------------------------------


def ecdsa_message_hash(signed_bytes: bytes) -> bytes:
    """The 32 bytes raw ECDSA actually covers, given what you hand to cosmos.

    Because cosmos-sdk's secp256k1 hashes its argument internally, the value the
    curve arithmetic consumes is one SHA-256 further along than the digest you
    passed in. Everything here passes a digest, so this is the second of the two
    hashes and the result is sha256(sha256(...)).

    Exposed as its own function for two reasons. It is the number to reach for
    when driving a raw ECDSA library — most of them, unlike cosmos, verify
    exactly the bytes you give them, so handing one the digest fails while Go
    insists the same signature is fine. And it is the value the oracle publishes
    as `response_ecdsa_message_hash_hex` and `receipt.ecdsa_message_hash_hex`,
    so a port can check the trap directly instead of inferring it from a
    rejection. verify_cosmos_signature() calls this rather than repeating it:
    one place computes it, so a test against the oracle tests the real path.
    """
    return hashlib.sha256(signed_bytes).digest()


def verify_cosmos_signature(pubkey_bz: bytes, signed_bytes: bytes, signature: bytes) -> bool:
    """Verify a 64-byte R||S secp256k1 signature the way cosmos-sdk does.

    Args:
        pubkey_bz: the supplier operator's 33-byte compressed public key.
        signed_bytes: what was passed to the signing key. For everything in this
            file that is already a 32-byte SHA-256 digest — see the note below.
        signature: 64 bytes, R || S, both big-endian.

    Returns:
        True if the signature is valid AND canonical. Never raises: a signature
        arrives from the network, so malformed is an answer, not an exception.

    THE DOUBLE HASH. cosmos-sdk's secp256k1 hashes whatever it is given before
    doing any curve arithmetic — `ecdsa.SignCompact(priv, crypto.Sha256(msg))`
    on the way in, `signature.Verify(crypto.Sha256(msg), pub)` on the way out
    (crypto/keys/secp256k1/secp256k1_nocgo.go). Callers here pass a digest, so
    the number the maths actually consumes is sha256(sha256(bytes)). Hash once
    and nothing verifies, with no hint as to why.

    LOW-S. For every valid (R, S) there is an equally valid (R, N-S), so a
    signature can be mutated by anyone into a different-looking signature that
    still verifies. Cosmos closes that off by rejecting S > N/2 outright, and so
    does this function. Leaving it out gives you a verifier that accepts
    signatures the chain will not — the one direction in which being wrong is
    unsafe rather than merely inconvenient.
    """
    if len(signature) != SIGNATURE_SIZE or len(pubkey_bz) != PUBKEY_SIZE:
        return False

    try:
        pubkey = decompress_point(pubkey_bz)
    except ValueError:
        return False

    r = int.from_bytes(signature[:32], "big")
    s = int.from_bytes(signature[32:], "big")

    # R and S must be scalars in [1, N). Cosmos reduces out-of-range values mod
    # N instead of rejecting them, so this is fractionally stricter than the
    # chain — in the safe direction, and only for encodings no signer produces.
    if not 1 <= r < N or not 1 <= s < N:
        return False
    # N is odd, so N // 2 is (N-1)/2: decred's IsOverHalfOrder boundary.
    if s > N // 2:
        return False

    # ...and here is the second hash.
    e = int.from_bytes(ecdsa_message_hash(signed_bytes), "big") % N

    w = pow(s, -1, N)
    point = point_add(scalar_base_mult(e * w % N), scalar_mult(r * w % N, pubkey))
    if point is None:
        return False

    # x is a field element mod P and r is a scalar mod N, and P > N, so the
    # comparison has to happen mod N.
    return point[0] % N == r


# --------------------------------------------------------------------------
# The two response checks
#
# Both are PIECES. Neither is a verification on its own, which is what the
# `_only` in the first one's name is there to keep saying. open_relay_response()
# at the bottom of this file composes them, in an order that is not cosmetic.
# --------------------------------------------------------------------------


def verify_response_signature_only(relay_response_bz: bytes, supplier_pubkey_bz: bytes) -> bool:
    """Verify the supplier operator's signature over a RelayResponse, and NOTHING else.

    NOT SUFFICIENT ON ITS OWN — hence the name. This proves the supplier produced
    a response with this session header and this payload HASH. It says nothing
    about the payload sitting next to it, because the payload is not what was
    signed. Pair it with verify_payload_binding(), or just call
    open_relay_response(), which is the function you actually want.

    Args:
        relay_response_bz: the marshalled RelayResponse, exactly as received.
        supplier_pubkey_bz: the supplier operator's 33-byte compressed public
            key, as published on chain. Fetching it from anywhere else — a
            header, the response itself — verifies nothing at all.

    Returns:
        True if the signature checks out. False on a bad signature and on
        malformed bytes alike.
    """
    try:
        response = parse_relay_response(relay_response_bz)
        signable_hash = response_signable_hash(relay_response_bz)
    except ValueError:
        return False

    return verify_cosmos_signature(
        supplier_pubkey_bz, signable_hash, response.supplier_operator_signature
    )


def verify_payload_binding(response: RelayResponse) -> bool:
    """Check the payload really is the one the signature committed to.

    payload_hash is inside the signed bytes; the payload is not. This is the
    step that connects them, and skipping it leaves the body swappable by
    anyone who can touch the response in flight.

    Returns True when SHA-256 of the payload equals payload_hash. A response
    with no payload_hash at all comes from a pre-v0.1.25 relayer, which signed
    the payload directly, so there is nothing to bind and this returns True.
    """
    if not response.payload_hash:
        return True
    if len(response.payload_hash) != SHA256_SIZE:
        return False
    return hashlib.sha256(response.payload).digest() == response.payload_hash


# --------------------------------------------------------------------------
# What is actually inside the payload
#
# RelayResponse.Payload is not your answer. It is a marshalled POKTHTTPResponse
# wrapping the backend's HTTP reply, and the JSON-RPC body is one layer further
# down in body_bz. Callers that treat the payload as JSON get a parse error on a
# perfectly good response.
# --------------------------------------------------------------------------


class HTTPResponse(NamedTuple):
    """The backend's HTTP reply, unwrapped from the relay response payload."""

    status_code: int
    #: Header name to its values. HTTP allows one name to carry several.
    headers: Dict[str, List[str]]
    #: The bytes the backend returned. For a JSON-RPC service, the JSON answer.
    body: bytes


def parse_pokt_http_response(payload: bytes) -> HTTPResponse:
    """Unwrap RelayResponse.Payload into the backend's status, headers and body.

    Args:
        payload: the `payload` field of a RelayResponse.

    Returns:
        The HTTP response the relayer received from the backend service.

    Raises:
        ValueError: if the payload is not a well-formed POKTHTTPResponse. Some
            transports put something else in there — a WebSocket subscription
            frame, for instance — so a caller that serves more than JSON-RPC
            should be ready to fall back to treating the payload as opaque.

    Note the status code is the BACKEND's. A relay can come back HTTP 200 at the
    outer layer and carry a 500 here, and a JSON-RPC service will often return
    200 here with the error in the body. Both are worth checking.
    """
    status_code = 0
    headers: Dict[str, List[str]] = {}
    body = b""

    for field in iter_fields(payload):
        if field.number == HTTP_STATUS_CODE and field.wire_type == WIRE_VARINT:
            status_code = field.varint
        elif field.number == HTTP_HEADER and field.wire_type == WIRE_LEN:
            name, values = _parse_header_entry(field.data)
            if name:
                headers.setdefault(name, []).extend(values)
        elif field.number == HTTP_BODY_BZ and field.wire_type == WIRE_LEN:
            body = field.data

    return HTTPResponse(status_code=status_code, headers=headers, body=body)


def _parse_header_entry(entry: bytes) -> Tuple[str, List[str]]:
    """Parse one `map<string, Header>` entry into (name, values).

    protobuf has no map on the wire: a map field is repeated occurrences of a
    synthetic two-field message, key=1 and value=2. Here the value is a Header,
    which repeats its own name in its `key` field. The map key is the one to
    trust — it is what a Go caller's `header[name]` lookup uses.
    """
    key_field = _first(entry, MAP_ENTRY_KEY)
    value_field = _first(entry, MAP_ENTRY_VALUE)
    if key_field is None or value_field is None:
        return "", []

    values = [
        sub.data.decode("utf-8")
        for sub in iter_fields(value_field.data)
        if sub.number == HEADER_VALUES and sub.wire_type == WIRE_LEN
    ]
    return key_field.data.decode("utf-8"), values


# --------------------------------------------------------------------------
# The relay receipt
#
# Opt in by sending `Pocket-Sign-Receipt: true` on the request; the relayer
# answers with `Pocket-Relay-Receipt`. Absent means the relayer did not sign
# one, which is what a relayer that has never heard of receipts also does — so
# a missing header is not evidence of anything.
# --------------------------------------------------------------------------

#: Restated from relayer/receipt.go rather than shared with it. A verifier that
#: imports the producer's constant cannot catch the producer changing it, which
#: is precisely the change a domain tag exists to make loud.
#:
#: 22 bytes, and the trailing NUL is part of them. It starts with 0x50, while
#: every other thing this key signs is a protobuf starting 0x0a — that is what
#: keeps a receipt from ever being mistaken for a claim, a proof, or a response.
RECEIPT_DOMAIN_TAG = b"POKT-RELAY-RECEIPT-v1\x00"

#: The header value is this prefix followed by 128 hex characters. The version
#: is also inside the signed digest, so a v2 receipt fails a v1 verifier
#: cleanly instead of being checked against the wrong contract.
RECEIPT_HEADER_PREFIX = "v1."


def receipt_digest(request_signature: bytes, response_payload_hash: bytes) -> bytes:
    """Build the 32 bytes the supplier key signs for a receipt.

        digest = sha256(domain tag || request signature || response payload hash)

    Args:
        request_signature: the ring signature YOU put on the request — the one
            sign.py produced. Reading it back out of the response would make the
            receipt prove nothing; the point is that it is a value only you and
            the supplier could pair with this response.
        response_payload_hash: the `payload_hash` field of the RelayResponse.

    Returns:
        32 bytes. Pass them to verify_cosmos_signature(), which will hash them
        again — see its docstring.

    Raises:
        ValueError: on an empty input. These are your own values, so an empty
            one is a bug in your code, not a hostile input.

    Why no length prefixes: exactly one field here is variable-length. The tag
    is 22 fixed bytes and the payload hash is 32, so the request signature's
    extent is pinned by the total length and no two distinct inputs can
    collide on a preimage. Add a second variable-length field and that stops
    being true, and every field then needs an explicit length.
    """
    if not request_signature:
        raise ValueError("request signature is empty")
    if not response_payload_hash:
        raise ValueError("response payload hash is empty")

    return hashlib.sha256(RECEIPT_DOMAIN_TAG + request_signature + response_payload_hash).digest()


def parse_receipt_header(header_value: str) -> bytes:
    """Pull the 64-byte signature out of a `Pocket-Relay-Receipt` header value.

    Args:
        header_value: the header as received, e.g. "v1.73bd7d91...".

    Returns:
        The 64-byte signature.

    Raises:
        ValueError: on a missing prefix, non-hex, or the wrong length.

    The relayer emits lowercase hex and a golden test pins that, but hex has no
    canonical case and an intermediary is free to change it, so this accepts
    either. Being liberal here costs nothing: the signature check downstream is
    what decides.
    """
    if not header_value.startswith(RECEIPT_HEADER_PREFIX):
        raise ValueError(
            f"receipt must start with {RECEIPT_HEADER_PREFIX!r}, got {header_value[:8]!r}"
        )

    hex_part = header_value[len(RECEIPT_HEADER_PREFIX) :]
    try:
        signature = bytes.fromhex(hex_part)
    except ValueError as exc:
        raise ValueError(f"receipt signature is not hex: {exc}") from None

    if len(signature) != SIGNATURE_SIZE:
        raise ValueError(f"receipt signature is {len(signature)} bytes, want {SIGNATURE_SIZE}")
    return signature


def verify_receipt(
    header_value: str,
    request_signature: bytes,
    response_payload_hash: bytes,
    supplier_pubkey_bz: bytes,
) -> bool:
    """Check that a receipt binds YOUR request to THIS response.

    Args:
        header_value: the `Pocket-Relay-Receipt` header, "v1." and 128 hex chars.
        request_signature: the ring signature you sent on the request.
        response_payload_hash: the `payload_hash` of the response you received.
        supplier_pubkey_bz: the supplier operator's 33-byte compressed public
            key, read from chain.

    Returns:
        True only if the supplier signed exactly this (request, response) pair.
        False for a malformed header, empty inputs, or a signature that does not
        check out — a receipt comes off the wire, so all three are answers.

    What a True means, precisely: the holder of the supplier operator key
    asserts that the response whose payload hashes to `response_payload_hash`
    was produced for the request carrying `request_signature`. That is a
    statement the supplier cannot later repudiate and cannot reuse for a
    different request, which is exactly what the RelayResponse signature alone
    could not give you.
    """
    if not request_signature or not response_payload_hash:
        return False

    try:
        signature = parse_receipt_header(header_value)
        digest = receipt_digest(request_signature, response_payload_hash)
    except ValueError:
        return False

    return verify_cosmos_signature(supplier_pubkey_bz, digest, signature)


# --------------------------------------------------------------------------
# The whole return trip
# --------------------------------------------------------------------------


def open_relay_response(
    relay_response_bz: bytes,
    supplier_pubkey_bz: bytes,
    request_signature: bytes = b"",
    receipt_header: Optional[str] = None,
) -> Tuple[Optional[HTTPResponse], str]:
    """Check everything a caller can check, and hand back the backend's answer.

    THIS is the function to copy. The order is not cosmetic:

        1. the supplier operator signature, over the filtered bytes;
        2. sha256(payload) == payload_hash -- WITHOUT THIS THE SIGNATURE CHECK
           MEANS NOTHING, because the payload is not in the signed bytes;
        3. the receipt, if one was asked for and returned, binding this response
           to the request that produced it;
        4. only then, parse the payload.

    Args:
        relay_response_bz: the marshalled RelayResponse, exactly as received.
        supplier_pubkey_bz: the supplier operator's 33-byte compressed public
            key, read from chain. Reading it out of the response verifies
            nothing at all.
        request_signature: the ring signature YOU put on the request. Only used
            for the receipt, so it can be left out when there is none.
        receipt_header: the `Pocket-Relay-Receipt` value, or None. A relayer
            returns one only when the request carried `Pocket-Sign-Receipt:
            true`, so None is normal. If you DID ask for one, treat its absence
            as a failure at the call site: this function cannot tell "not
            requested" from "dropped in transit".

    Returns:
        (http, reason). On success, the backend's unwrapped HTTP reply and an
        empty reason. On failure, None and the step that failed, so a rejection
        is debuggable rather than just a False. Never raises: every input here
        comes off the network, so malformed is an answer.
    """
    try:
        response = parse_relay_response(relay_response_bz)
    except ValueError as exc:
        return None, f"not a well-formed RelayResponse: {exc}"

    if len(response.supplier_operator_signature) != SIGNATURE_SIZE:
        return None, (
            f"supplier operator signature is {len(response.supplier_operator_signature)} "
            f"bytes, want {SIGNATURE_SIZE}"
        )

    if not verify_response_signature_only(relay_response_bz, supplier_pubkey_bz):
        return None, "supplier operator signature does not verify"

    if not verify_payload_binding(response):
        return None, "payload does not hash to payload_hash: the body was substituted"

    if receipt_header is not None and not verify_receipt(
        receipt_header, request_signature, response.payload_hash, supplier_pubkey_bz
    ):
        return None, "receipt does not bind this response to that request"

    try:
        return parse_pokt_http_response(response.payload), ""
    except ValueError as exc:
        # Not every transport wraps a POKTHTTPResponse -- a WebSocket
        # subscription frame does not -- so this is a legitimate outcome for a
        # response whose signature and receipt both checked out.
        return None, f"payload is not a POKTHTTPResponse: {exc}"


# --------------------------------------------------------------------------
# Demo: run the checks against the oracle's vectors and print what happened
#
#     go build -o /tmp/oracle ../oracle/
#     /tmp/oracle response-vectors | python3 verify.py
#
# For the pass/fail gate, use verify_against_oracle.py instead.
# --------------------------------------------------------------------------


def _report(vectors: dict) -> bool:
    """Run every check in this file over one `oracle response-vectors` document."""
    supplier_pub = bytes.fromhex(vectors["supplier"]["pub_hex"])
    relay_response_bz = bytes.fromhex(vectors["relay_response_hex"])
    receipt = vectors["receipt"]
    controls = vectors["negative_controls"]

    ok = True

    def check(label: str, passed: bool, detail: str = "") -> bool:
        """Record one line of the report. Returns what it was given."""
        nonlocal ok
        ok &= passed
        print(f"  [{'ok' if passed else 'FAIL'}] {label}{'  ' + detail if detail else ''}")
        return passed

    response = parse_relay_response(relay_response_bz)
    header = response.session_header

    print(f"RelayResponse             {len(relay_response_bz)} bytes")
    # A RelayResponse without a session header does not pass the relayer's own
    # ValidateBasic, so a caller will not meet one -- but the parse returns None
    # for it, and the rest of this report reads the header, so say so out loud.
    if check("carries a session header", header is not None) and header is not None:
        print(f"  application             {header.application_address}")
        print(f"  service                 {header.service_id}")
        print(f"  session                 {header.session_id}")
        print(
            f"  blocks                  {header.session_start_block_height}"
            f" -> {header.session_end_block_height}"
        )
    if response.relay_miner_error is not None:
        err = response.relay_miner_error
        print(f"  relay miner error       {err.codespace}/{err.code}: {err.description}")

    print("\nsignature over the response")
    signable = filter_signable_bytes(relay_response_bz)
    check(
        "filtered signable bytes match the oracle",
        signable.hex() == vectors["signable_bytes_hex"],
        f"{len(signable)} bytes, {len(relay_response_bz) - len(signable)} filtered out",
    )
    check("payload hashes to payload_hash", verify_payload_binding(response))
    # The double hash, checked against the number the oracle publishes rather
    # than inferred from the signature happening to verify.
    check(
        "the hash raw ECDSA covers is sha256 of that again",
        ecdsa_message_hash(response_signable_hash(relay_response_bz)).hex()
        == vectors["response_ecdsa_message_hash_hex"],
    )
    check(
        "supplier operator signature verifies",
        verify_response_signature_only(relay_response_bz, supplier_pub),
    )

    # Being unable to fail is the failure mode a signature check hides best. The
    # two ways a response can be altered are caught by two different mechanisms,
    # so flip a bit in each place and watch both of them fire.
    def flip_a_bit(needle: bytes) -> bytes:
        """The response with one bit of `needle` inverted, wherever it sits."""
        at = relay_response_bz.find(needle)
        if at < 0:
            raise ValueError(f"{needle!r} is not in this response")
        edited = bytearray(relay_response_bz)
        edited[at] ^= 0x01
        return bytes(edited)

    check(
        "...and rejects an edited payload (the hash binding catches it)",
        open_relay_response(flip_a_bit(response.payload), supplier_pub)[0] is None,
    )
    if header is not None:
        check(
            "...and rejects an edited session header (the signature catches it)",
            open_relay_response(flip_a_bit(header.service_id.encode()), supplier_pub)[0] is None,
        )

    print("\nthe backend's answer, two layers down")
    http = parse_pokt_http_response(response.payload)
    print(f"  status                  {http.status_code}")
    for name, values in sorted(http.headers.items()):
        print(f"  header                  {name}: {', '.join(values)}")
    print(f"  body                    {http.body.decode('utf-8', 'replace')}")
    inner = vectors["inner_pokt_http_response"]
    check("status matches the oracle", http.status_code == inner["status_code"])
    check("body matches the oracle", http.body.hex() == inner["body_bz_hex"])
    check("headers match the oracle", http.headers == inner["headers"])

    print("\nreceipt")
    request_signature = bytes.fromhex(receipt["request_signature_hex"])
    digest = receipt_digest(request_signature, response.payload_hash)
    check("digest matches the oracle", digest.hex() == receipt["digest_hex"])
    check(
        "the hash raw ECDSA covers matches the oracle",
        ecdsa_message_hash(digest).hex() == receipt["ecdsa_message_hash_hex"],
    )
    check(
        "receipt verifies against the supplier key",
        verify_receipt(
            receipt["header_value"], request_signature, response.payload_hash, supplier_pub
        ),
    )

    # The receipt is only worth something if it fails when either half of the
    # pair it commits to is swapped. Demonstrated, not asserted in prose.
    check(
        "...and fails for a different response",
        not verify_receipt(
            receipt["header_value"],
            request_signature,
            bytes.fromhex(controls["other_payload_hash_hex"]),
            supplier_pub,
        ),
    )
    check(
        "...and fails for a different request",
        not verify_receipt(
            receipt["header_value"],
            bytes.fromhex(controls["other_request_signature_hex"]),
            response.payload_hash,
            supplier_pub,
        ),
    )

    # (r, N-s) is as valid as (r, s) to the curve arithmetic; cosmos rejects it
    # on the low-S rule alone, and so does verify_cosmos_signature.
    signature = parse_receipt_header(receipt["header_value"])
    negated_s = (N - int.from_bytes(signature[32:], "big")) % N
    high_s = signature[:32] + negated_s.to_bytes(32, "big")
    check(
        "...and fails when S is negated (the low-S rule)",
        not verify_cosmos_signature(supplier_pub, digest, high_s),
    )

    # Everything above took the response apart. This is the one call a caller
    # actually makes: all four steps, in order, and the backend's answer out the
    # other side.
    print("\nthe whole return trip, in one call")
    opened, reason = open_relay_response(
        relay_response_bz, supplier_pub, request_signature, receipt["header_value"]
    )
    check("open_relay_response accepts response + receipt", opened is not None, reason)
    check(
        "...and returns the backend's own body",
        opened is not None and opened.body.hex() == inner["body_bz_hex"],
    )
    check(
        "...and refuses a receipt from a different relay",
        open_relay_response(
            relay_response_bz,
            supplier_pub,
            bytes.fromhex(controls["other_request_signature_hex"]),
            receipt["header_value"],
        )[0]
        is None,
    )

    return ok


if __name__ == "__main__":
    import argparse
    import json
    import subprocess
    import sys

    parser = argparse.ArgumentParser(
        description="Verify a signed RelayResponse and its receipt.",
        epilog="Reads `oracle response-vectors` JSON on stdin, or runs the oracle itself.",
    )
    parser.add_argument(
        "--oracle",
        default="/tmp/oracle",
        help="oracle binary to run when stdin is a terminal (default /tmp/oracle)",
    )
    args = parser.parse_args()

    if sys.stdin.isatty():
        try:
            stdout = subprocess.run(
                [args.oracle, "response-vectors"],
                capture_output=True,
                check=True,
                text=True,
            ).stdout
        except (OSError, subprocess.CalledProcessError) as exc:
            print(
                f"cannot run the oracle ({exc}). Build it with:\n\n"
                "    go build -o /tmp/oracle ../oracle/\n\n"
                "then re-run, or pipe its output in:\n\n"
                "    /tmp/oracle response-vectors | python3 verify.py",
                file=sys.stderr,
            )
            sys.exit(2)
    else:
        stdout = sys.stdin.read()

    passed = _report(json.loads(stdout))
    print("\n" + ("PASS" if passed else "FAIL"))
    sys.exit(0 if passed else 1)
