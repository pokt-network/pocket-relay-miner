//! Checking what comes BACK from a relayer: the response signature, and the
//! optional relay receipt.
//!
//! [`crate`] signs a relay request. This module is the return trip. It is
//! written against the same authority: "correct" means the bytes agree with
//! what the relayer and the cosmos verifier produce, which is what
//! `../oracle response-vectors` emits from the real poktroll and shannon-sdk
//! types. Round-tripping your own code proves nothing here either.
//!
//! # What a caller actually receives
//!
//! A marshalled `RelayResponse`:
//!
//! ```text
//! RelayResponse
//!   1 meta          RelayResponseMetadata
//!                     1 session_header
//!                     2 supplier_operator_signature   <- 64 bytes, R||S
//!   2 payload       bytes  <- a marshalled POKTHTTPResponse
//!   3 relay_miner_error
//!   4 payload_hash  bytes  <- sha256(payload)
//! ```
//!
//! and, if the request carried `Pocket-Sign-Receipt: true`, a
//! `Pocket-Relay-Receipt` header.
//!
//! # The four traps
//!
//! 1. **The payload is NOT signed. Its hash is.** The supplier signs the
//!    response with field 2 removed, so a valid signature says nothing about
//!    the body you are holding. You must hash the payload yourself and compare
//!    it to field 4 — [`open_relay_response`] does; a hand-rolled check that
//!    stops at "signature verified" is decorative.
//!
//! 2. **The signable bytes are obtained by FILTERING, never by re-encoding.**
//!    See [`signable_bytes`].
//!
//! 3. **Everything is hashed twice.** The supplier signs
//!    `sha256(signable_bytes)`, and cosmos's secp256k1 hashes whatever it is
//!    handed one more time before ECDSA sees it. The message hash is therefore
//!    `sha256(sha256(signable_bytes))`. Same for the receipt. See
//!    [`verify_cosmos_secp256k1`].
//!
//! 4. **Low-S is mandatory.** Cosmos rejects `S > N/2` at parse time, so a
//!    signature this module accepts but cosmos would not is a divergence that
//!    only shows up against a real supplier. See [`VerifyError::HighS`].
//!
//! # Protobuf without a protobuf crate
//!
//! Everything below parses the wire format by hand, for two reasons. Filtering
//! (trap 2) needs the received bytes, which a generated struct throws away.
//! And a reader porting this to another language gets the actual contract —
//! field numbers and wire types — instead of a `.proto` they then have to go
//! and find.

use k256::ecdsa::signature::hazmat::PrehashVerifier;
use k256::ecdsa::{Signature, VerifyingKey};
use k256::elliptic_curve::scalar::IsHigh;
use k256::sha2::{Digest, Sha256};
use thiserror::Error;

// ---------------------------------------------------------------------------
// The contract, as constants
// ---------------------------------------------------------------------------

/// Field numbers in `RelayResponse` (poktroll `proto/pocket/service/relay.proto`).
mod field {
    pub const META: u32 = 1;
    pub const PAYLOAD: u32 = 2;
    pub const PAYLOAD_HASH: u32 = 4;
}

/// Field numbers in `RelayResponseMetadata`.
mod meta_field {
    pub const SUPPLIER_OPERATOR_SIGNATURE: u32 = 2;
}

/// Field numbers in `POKTHTTPResponse` and its header map
/// (shannon-sdk `proto/types/http.proto`).
mod http_field {
    pub const STATUS_CODE: u32 = 1;
    pub const HEADER: u32 = 2;
    pub const BODY_BZ: u32 = 3;

    /// A `map<string, Header>` entry is a message with the key at 1 and the
    /// value at 2. That is the protobuf spec, not a poktroll choice.
    pub const MAP_KEY: u32 = 1;
    pub const MAP_VALUE: u32 = 2;

    /// `Header` — protobuf has no `map<string, repeated string>`, hence the
    /// key appearing twice: once as the map key, once inside `Header`.
    pub const HEADER_KEY: u32 = 1;
    pub const HEADER_VALUES: u32 = 2;
}

/// The receipt's domain separator, restated rather than shared.
///
/// It MUST equal `receiptDomainTag` in `relayer/receipt.go`, and it is written
/// out here on purpose: a verifier that imports the producer's constant cannot
/// catch the producer changing it, which is the one failure a versioned domain
/// tag exists to catch.
///
/// Twenty-two bytes including the trailing NUL. The `v1` versions the contract
/// *inside* the signed bytes: change what enters the digest and a v1 verifier
/// fails cleanly instead of quietly verifying different content.
pub const RECEIPT_DOMAIN_TAG: &[u8] = b"POKT-RELAY-RECEIPT-v1\0";

// The tag's length is load-bearing, not decorative: [`receipt_digest`]'s
// preimage has no length prefixes, and it can only get away with that because
// the tag and the payload hash are both fixed size. Pinning it here means a
// future edit to the string is a compile error rather than a silently
// weaker binding.
const _: () = assert!(RECEIPT_DOMAIN_TAG.len() == 22);

/// The response header carrying the receipt.
pub const RECEIPT_HEADER_NAME: &str = "Pocket-Relay-Receipt";

/// The request header that asks for one. Absent means no, so a caller that
/// does not know about receipts is never charged for one.
pub const RECEIPT_REQUEST_HEADER_NAME: &str = "Pocket-Sign-Receipt";

/// The only receipt header version this module understands.
const RECEIPT_HEADER_PREFIX: &str = "v1.";

/// secp256k1 signatures are 64 bytes: R and S, big-endian, no DER.
const SIGNATURE_LEN: usize = 64;

/// Public keys are 33-byte compressed SEC1, as cosmos stores them.
const COMPRESSED_PUBKEY_LEN: usize = 33;

/// sha256 output, and therefore the length of `payload_hash`.
const HASH_LEN: usize = 32;

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/// Why a response, or a receipt, was not accepted.
///
/// Deliberately fine-grained: "invalid signature" and "the payload you were
/// handed is not the payload that was signed" are the same outcome and
/// completely different bugs.
#[derive(Error, Debug, Clone, PartialEq, Eq)]
pub enum VerifyError {
    /// A length prefix ran past the end of the buffer.
    #[error("truncated protobuf: field at offset {offset} needs {need} bytes, {have} remain")]
    Truncated {
        offset: usize,
        need: usize,
        have: usize,
    },
    /// A varint had no terminating byte, or did not fit in 64 bits.
    #[error("malformed varint at offset {0}")]
    MalformedVarint(usize),
    /// A wire type this schema does not use.
    ///
    /// Two cases. Wire types 3 and 4 are the deprecated group encoding, which
    /// nothing here emits and which cannot be skipped without tracking nesting
    /// depth. And a field that must be `bytes` or a message — `meta`,
    /// `payload`, `payload_hash` — arriving as a varint is not a
    /// `RelayResponse`, whatever else it may be.
    #[error("field {number} has wire type {wire_type}, which this schema does not use here")]
    UnsupportedWireType { number: u32, wire_type: u8 },
    /// A field this module rewrites or reads appeared more than once.
    ///
    /// Protobuf permits it (last one wins, or messages merge), and gogoproto
    /// never emits it. Emulating the merge would mean re-encoding, which is
    /// exactly what [`signable_bytes`] must not do — so refuse instead of
    /// producing bytes that quietly differ from the producer's.
    #[error("field {0} appears more than once")]
    DuplicateField(u32),
    /// No `meta` (field 1). Not reachable from the relayer — the field is
    /// `nullable = false`, so gogoproto always writes it.
    #[error("RelayResponse has no meta (field 1)")]
    MissingMeta,
    /// `meta.supplier_operator_signature` (field 2 of field 1) is absent.
    #[error("RelayResponse carries no supplier operator signature")]
    MissingSignature,
    /// No `payload` (field 2).
    #[error("RelayResponse has no payload (field 2)")]
    MissingPayload,
    /// No `payload_hash` (field 4).
    #[error("RelayResponse has no payload hash (field 4)")]
    MissingPayloadHash,
    /// The payload does not hash to the `payload_hash` that was signed.
    ///
    /// The signature is valid and the body is not the one it covers: either
    /// the payload was replaced in transit, or it belongs to a different
    /// response.
    #[error("payload does not match the signed payload hash")]
    PayloadHashMismatch,
    /// `payload_hash` was not 32 bytes.
    #[error("payload hash must be {HASH_LEN} bytes, got {0}")]
    PayloadHashLength(usize),
    #[error("signature must be {SIGNATURE_LEN} bytes of R||S, got {0}")]
    SignatureLength(usize),
    /// R or S was zero or out of range.
    #[error("signature scalars are out of range")]
    SignatureMalformed,
    #[error("public key must be {COMPRESSED_PUBKEY_LEN} compressed bytes, got {0}")]
    PublicKeyLength(usize),
    #[error("public key is not a point on secp256k1")]
    PublicKeyInvalid,
    /// `S > N/2`. Cryptographically the signature may be perfectly valid — ECDSA
    /// is malleable, so `(R, N-S)` verifies wherever `(R, S)` does. Cosmos
    /// refuses both halves of that pair by policy, so this must too, or a
    /// verifier accepts responses the network considers unsigned.
    #[error("signature has a high S; cosmos rejects malleable signatures")]
    HighS,
    #[error("signature does not verify under the supplier operator key")]
    BadSignature,
    #[error("receipt header must begin with {RECEIPT_HEADER_PREFIX:?}")]
    ReceiptVersion,
    #[error("receipt header must carry {} hex characters, got {got}", SIGNATURE_LEN * 2)]
    ReceiptLength { got: usize },
    #[error("receipt header is not hex")]
    ReceiptNotHex,
    /// The receipt binds the request signature, so there must be one.
    #[error("receipt needs the request signature it binds, got none")]
    ReceiptEmptyRequestSignature,
    /// A `POKTHTTPResponse` string field was not valid UTF-8.
    #[error("POKTHTTPResponse contains a non-UTF-8 string")]
    NotUtf8,
    /// `status_code` is a `uint32`.
    #[error("status code {0} does not fit in a uint32")]
    StatusCodeRange(u64),
}

// ---------------------------------------------------------------------------
// Protobuf wire format, the little of it that is needed
// ---------------------------------------------------------------------------

/// One field of a protobuf message, kept as the exact bytes it occupied.
///
/// `raw` is the whole record — tag, length prefix, value — and it is what
/// makes filtering byte-exact: a kept field is copied, never re-encoded.
#[derive(Debug, Clone, Copy)]
struct Record<'a> {
    number: u32,
    wire_type: u8,
    raw: &'a [u8],
    /// The value alone. For wire type 2 this is the length-delimited payload;
    /// for a varint it is the varint's own bytes.
    value: &'a [u8],
}

impl Record<'_> {
    /// Decodes a varint field's value. Only valid for wire type 0.
    fn varint(&self) -> Result<u64, VerifyError> {
        let mut pos = 0;
        read_varint(self.value, &mut pos)
    }

    fn utf8(&self) -> Result<String, VerifyError> {
        core::str::from_utf8(length_delimited(self)?)
            .map(str::to_owned)
            .map_err(|_| VerifyError::NotUtf8)
    }
}

/// Reads a base-128 varint, advancing `pos`.
fn read_varint(buf: &[u8], pos: &mut usize) -> Result<u64, VerifyError> {
    let start = *pos;
    let mut value = 0u64;

    // Ten groups of 7 bits is 70, the most that can encode a u64. Bounding the
    // loop is what stops a hostile 0x80-repeating buffer from spinning.
    for shift in (0..70).step_by(7) {
        let byte = *buf.get(*pos).ok_or(VerifyError::MalformedVarint(start))?;
        *pos += 1;
        value |= u64::from(byte & 0x7f) << shift;
        if byte & 0x80 == 0 {
            return Ok(value);
        }
    }
    Err(VerifyError::MalformedVarint(start))
}

/// Emits a varint. The only re-encoding in this module — see [`signable_bytes`].
fn write_varint(out: &mut Vec<u8>, mut value: u64) {
    loop {
        let byte = (value & 0x7f) as u8;
        value >>= 7;
        if value == 0 {
            out.push(byte);
            return;
        }
        out.push(byte | 0x80);
    }
}

/// Splits a message into its top-level records, in wire order.
fn parse_records(buf: &[u8]) -> Result<Vec<Record<'_>>, VerifyError> {
    let mut records = Vec::new();
    let mut pos = 0;

    while pos < buf.len() {
        let start = pos;
        let tag = read_varint(buf, &mut pos)?;
        let number = u32::try_from(tag >> 3).map_err(|_| VerifyError::MalformedVarint(start))?;
        let wire_type = (tag & 0x7) as u8;

        let value_start;
        let value_end;
        match wire_type {
            0 => {
                value_start = pos;
                read_varint(buf, &mut pos)?;
                value_end = pos;
            }
            1 | 5 => {
                let len = if wire_type == 1 { 8 } else { 4 };
                value_start = pos;
                value_end = take(buf, pos, len, start)?;
                pos = value_end;
            }
            2 => {
                // A length that does not fit a usize cannot be satisfied by a
                // buffer that does, so saturating here is not a lost case:
                // `take` turns it into the truncation it already is.
                let len = usize::try_from(read_varint(buf, &mut pos)?).unwrap_or(usize::MAX);
                value_start = pos;
                value_end = take(buf, pos, len, start)?;
                pos = value_end;
            }
            _ => return Err(VerifyError::UnsupportedWireType { number, wire_type }),
        }

        records.push(Record {
            number,
            wire_type,
            raw: &buf[start..pos],
            value: &buf[value_start..value_end],
        });
    }

    Ok(records)
}

/// Bounds check for a fixed-size read, reported against the record's start so
/// the error points at the field rather than at the byte that ran out.
fn take(buf: &[u8], pos: usize, len: usize, record_start: usize) -> Result<usize, VerifyError> {
    let end = pos.checked_add(len).ok_or(VerifyError::Truncated {
        offset: record_start,
        need: len,
        have: buf.len() - pos,
    })?;
    if end > buf.len() {
        return Err(VerifyError::Truncated {
            offset: record_start,
            need: len,
            have: buf.len() - pos,
        });
    }
    Ok(end)
}

/// The single record with this field number, or `None`.
fn unique<'r, 'a>(
    records: &'r [Record<'a>],
    number: u32,
) -> Result<Option<&'r Record<'a>>, VerifyError> {
    let mut found = None;
    for record in records.iter().filter(|r| r.number == number) {
        if found.is_some() {
            return Err(VerifyError::DuplicateField(number));
        }
        found = Some(record);
    }
    Ok(found)
}

/// The value of a field that must be length-delimited.
///
/// Every field this module reads is `bytes` or a nested message. Accepting a
/// varint where one of those belongs would mean parsing a number's own bytes as
/// a message and reporting whatever fell out.
fn length_delimited<'a>(record: &Record<'a>) -> Result<&'a [u8], VerifyError> {
    if record.wire_type != 2 {
        return Err(VerifyError::UnsupportedWireType {
            number: record.number,
            wire_type: record.wire_type,
        });
    }
    Ok(record.value)
}

// ---------------------------------------------------------------------------
// Reading a RelayResponse
// ---------------------------------------------------------------------------

/// The `payload` (field 2): a marshalled `POKTHTTPResponse`.
///
/// Not covered by the signature. See [`payload_hash`].
pub fn payload(relay_response: &[u8]) -> Result<&[u8], VerifyError> {
    let records = parse_records(relay_response)?;
    length_delimited(unique(&records, field::PAYLOAD)?.ok_or(VerifyError::MissingPayload)?)
}

/// The `payload_hash` (field 4): `sha256(payload)`, and the only thing about
/// the payload the signature actually covers.
pub fn payload_hash(relay_response: &[u8]) -> Result<&[u8], VerifyError> {
    let records = parse_records(relay_response)?;
    let hash = length_delimited(
        unique(&records, field::PAYLOAD_HASH)?.ok_or(VerifyError::MissingPayloadHash)?,
    )?;
    if hash.len() != HASH_LEN {
        return Err(VerifyError::PayloadHashLength(hash.len()));
    }
    Ok(hash)
}

/// The `meta.supplier_operator_signature` (field 2 of field 1): 64 bytes, R||S.
pub fn supplier_operator_signature(relay_response: &[u8]) -> Result<&[u8], VerifyError> {
    let records = parse_records(relay_response)?;
    let meta = unique(&records, field::META)?.ok_or(VerifyError::MissingMeta)?;
    let meta_records = parse_records(length_delimited(meta)?)?;
    let signature = length_delimited(
        unique(&meta_records, meta_field::SUPPLIER_OPERATOR_SIGNATURE)?
            .ok_or(VerifyError::MissingSignature)?,
    )?;
    if signature.len() != SIGNATURE_LEN {
        return Err(VerifyError::SignatureLength(signature.len()));
    }
    Ok(signature)
}

/// Rebuilds the bytes the supplier signed, by **filtering the bytes you
/// received**.
///
/// The supplier signs the `RelayResponse` with two fields cleared:
/// `meta.supplier_operator_signature` (it cannot sign over itself) and
/// `payload` (so an on-chain proof can carry the signature without the body —
/// `payload_hash` stands in for it). Everything else, including fields this
/// module does not know about, is part of the signed message.
///
/// # Why filtering, and not "parse then re-encode"
///
/// Re-encoding is the obvious implementation and it is wrong. Protobuf has no
/// canonical form: field order, varint minimality, and how a library handles
/// fields it does not know about are all free choices, and your encoder does
/// not have to make the same ones gogoproto did. Every difference changes the
/// hash and every signature fails, or — worse — fails only for the responses
/// that happen to contain the field you re-encode differently.
///
/// Filtering sidesteps all of it. Each field's encoding is self-contained, so
/// concatenating the records you keep reproduces the producer's bytes exactly.
///
/// One length prefix must be rewritten: dropping the signature from inside
/// `meta` shortens `meta`, so its own prefix changes. That is the only byte
/// this function invents, and it is safe because gogoproto — like every
/// protobuf implementation in practice — emits minimal varints, which is what
/// [`write_varint`] produces.
///
/// # The backwards-compatibility rule
///
/// `payload` is dropped **only if `payload_hash` is present**. `payload_hash`
/// arrived in poktroll v0.1.25; a supplier older than that signs the payload
/// itself, and dropping it would make every one of those responses fail. This
/// mirrors `RelayResponse.GetSignableBytesHash` in poktroll, which is
/// conditional for the same reason.
pub fn signable_bytes(relay_response: &[u8]) -> Result<Vec<u8>, VerifyError> {
    let records = parse_records(relay_response)?;

    let meta = unique(&records, field::META)?.ok_or(VerifyError::MissingMeta)?;
    let drop_payload = unique(&records, field::PAYLOAD_HASH)?.is_some();
    // Reject a duplicate payload even when it is about to be dropped: two of
    // them means the received bytes are not what any gogoproto marshaller
    // produced, and the rest of this function's reasoning does not hold.
    unique(&records, field::PAYLOAD)?;

    let meta_value = length_delimited(meta)?;
    let meta_records = parse_records(meta_value)?;
    unique(&meta_records, meta_field::SUPPLIER_OPERATOR_SIGNATURE)?;
    let mut filtered_meta = Vec::with_capacity(meta_value.len());
    for record in &meta_records {
        if record.number != meta_field::SUPPLIER_OPERATOR_SIGNATURE {
            filtered_meta.extend_from_slice(record.raw);
        }
    }

    let mut out = Vec::with_capacity(relay_response.len());
    for record in &records {
        match record.number {
            field::PAYLOAD if drop_payload => {}
            field::META => {
                // Tag for field 1, wire type 2. Re-emitted because the length
                // that follows it changed.
                write_varint(&mut out, (u64::from(field::META) << 3) | 2);
                write_varint(&mut out, filtered_meta.len() as u64);
                out.extend_from_slice(&filtered_meta);
            }
            _ => out.extend_from_slice(record.raw),
        }
    }

    Ok(out)
}

/// `sha256(signable_bytes)` — the 32 bytes the supplier's key was handed.
///
/// Note "handed", not "hashed": cosmos hashes them again. See
/// [`verify_cosmos_secp256k1`].
pub fn signable_bytes_hash(relay_response: &[u8]) -> Result<[u8; HASH_LEN], VerifyError> {
    Ok(sha256(&signable_bytes(relay_response)?))
}

// ---------------------------------------------------------------------------
// The signature check
// ---------------------------------------------------------------------------

/// Verifies a cosmos `secp256k1` signature over `message`.
///
/// **`message` is hashed here.** Cosmos's `PrivKey.Sign` does
/// `ecdsa.SignCompact(priv, sha256(msg))` and `PubKey.VerifySignature` does
/// `sig.Verify(sha256(msg), pub)`, so the ECDSA message hash is one sha256
/// deeper than whatever the caller thought it was signing. Since both callers
/// in this module already pass a digest, the real message hash is
/// `sha256(sha256(...))`.
///
/// This is the single easiest thing to get wrong in a port, and it fails
/// silently in the sense that matters: every signature is rejected, with no
/// hint as to which of the two layers is missing.
///
/// Low-S is enforced explicitly rather than left to the crate. k256 does
/// reject high-S — `Secp256k1::NORMALIZE_S` is `true`, so
/// `ecdsa::hazmat::verify_prehashed` refuses it — but it reports the same
/// opaque error as a wrong signature, and a policy this load-bearing should
/// not be an implementation detail of a dependency that could relax it.
pub fn verify_cosmos_secp256k1(
    message: &[u8],
    signature: &[u8],
    compressed_pubkey: &[u8],
) -> Result<(), VerifyError> {
    if signature.len() != SIGNATURE_LEN {
        return Err(VerifyError::SignatureLength(signature.len()));
    }
    if compressed_pubkey.len() != COMPRESSED_PUBKEY_LEN {
        return Err(VerifyError::PublicKeyLength(compressed_pubkey.len()));
    }

    let signature =
        Signature::from_slice(signature).map_err(|_| VerifyError::SignatureMalformed)?;
    if signature.s().is_high().into() {
        return Err(VerifyError::HighS);
    }

    let verifying_key = VerifyingKey::from_sec1_bytes(compressed_pubkey)
        .map_err(|_| VerifyError::PublicKeyInvalid)?;

    verifying_key
        .verify_prehash(&sha256(message), &signature)
        .map_err(|_| VerifyError::BadSignature)
}

/// Verifies the supplier operator's signature over the `RelayResponse`.
///
/// This proves the supplier produced this response in this session. It does
/// **not** prove the payload you are holding is the one it covers (trap 1), and
/// it does not say which request the response answers — that is what the
/// receipt is for. See [`open_relay_response`] for both.
pub fn verify_response_signature(
    relay_response: &[u8],
    supplier_pubkey: &[u8],
) -> Result<(), VerifyError> {
    let signature = supplier_operator_signature(relay_response)?;
    let signable_hash = signable_bytes_hash(relay_response)?;
    verify_cosmos_secp256k1(&signable_hash, signature, supplier_pubkey)
}

// ---------------------------------------------------------------------------
// The receipt
// ---------------------------------------------------------------------------

/// The 32 bytes a receipt signs:
/// `sha256(domain_tag || request_signature || response_payload_hash)`.
///
/// # Why it exists
///
/// A response signature covers a session header shared by thousands of relays,
/// so it cannot answer "which request does this answer?". The receipt binds the
/// request's ring signature — unique per relay — to the response's payload
/// hash, and both ends of that binding are things the caller already has: it
/// sent the first and just verified the second.
///
/// # The encoding invariant
///
/// Exactly one variable-length field, so no length prefixes are needed: the tag
/// is 22 fixed bytes and the payload hash is 32, which pins the request
/// signature's extent by subtraction. That is why this function insists on a
/// 32-byte hash — accept a short one and two different `(request, response)`
/// pairs could produce the same preimage, which is precisely the forgery the
/// receipt is supposed to prevent.
pub fn receipt_digest(
    request_signature: &[u8],
    response_payload_hash: &[u8],
) -> Result<[u8; HASH_LEN], VerifyError> {
    if request_signature.is_empty() {
        return Err(VerifyError::ReceiptEmptyRequestSignature);
    }
    if response_payload_hash.len() != HASH_LEN {
        return Err(VerifyError::PayloadHashLength(response_payload_hash.len()));
    }

    let mut hasher = Sha256::new();
    hasher.update(RECEIPT_DOMAIN_TAG);
    hasher.update(request_signature);
    hasher.update(response_payload_hash);
    Ok(hasher.finalize().into())
}

/// Parses a `Pocket-Relay-Receipt` value: `v1.` followed by 128 hex characters.
///
/// The version prefix is matched exactly. Being liberal there would mean
/// accepting a `v2.` receipt as a v1 one, and the whole point of versioning
/// inside the signed bytes is that a verifier which does not understand the
/// contract says so instead of guessing. The hex is accepted in either case;
/// the relayer emits lowercase, but nothing signs the header's spelling.
pub fn parse_receipt_header(header_value: &str) -> Result<Vec<u8>, VerifyError> {
    let hex_part = header_value
        .strip_prefix(RECEIPT_HEADER_PREFIX)
        .ok_or(VerifyError::ReceiptVersion)?;

    if hex_part.len() != SIGNATURE_LEN * 2 {
        return Err(VerifyError::ReceiptLength {
            got: hex_part.len(),
        });
    }
    hex::decode(hex_part).map_err(|_| VerifyError::ReceiptNotHex)
}

/// Verifies a receipt against the request you sent and the response you got.
///
/// `request_signature` is the 199-byte ring signature from your own
/// `RelayRequest` — the one [`crate::build_relay_signature`] produced. Passing
/// anything else, including a signature from a different relay, must fail; that
/// failure is the entire proof.
///
/// `response_payload_hash` must come from the response whose signature you have
/// already verified. Hashing the payload yourself and passing that instead
/// verifies nothing: an attacker who swapped the body would have swapped the
/// hash you computed from it too.
pub fn verify_receipt(
    header_value: &str,
    request_signature: &[u8],
    response_payload_hash: &[u8],
    supplier_pubkey: &[u8],
) -> Result<(), VerifyError> {
    let signature = parse_receipt_header(header_value)?;
    let digest = receipt_digest(request_signature, response_payload_hash)?;
    verify_cosmos_secp256k1(&digest, &signature, supplier_pubkey)
}

// ---------------------------------------------------------------------------
// Reaching the answer: RelayResponse.Payload is a POKTHTTPResponse
// ---------------------------------------------------------------------------

/// The backend's HTTP answer, as the relayer wrapped it.
///
/// `RelayResponse.Payload` is not the JSON-RPC body: it is a marshalled
/// `POKTHTTPResponse`, and the body is one layer further in. A caller that
/// hands `Payload` straight to a JSON parser gets a parse error and blames the
/// backend.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PoktHttpResponse {
    /// The backend's HTTP status. For JSON-RPC this is 200 even when the
    /// answer is an error — the error is in the body.
    pub status_code: u32,
    /// Header keys with their values. A protobuf map has no defined order, so
    /// neither does this: look keys up, do not index.
    pub headers: Vec<(String, Vec<String>)>,
    /// The bytes the backend returned. The JSON-RPC answer, if any, is here.
    pub body: Vec<u8>,
}

impl PoktHttpResponse {
    /// Case-insensitive header lookup, because HTTP header names are.
    pub fn header(&self, name: &str) -> Option<&[String]> {
        self.headers
            .iter()
            .find(|(key, _)| key.eq_ignore_ascii_case(name))
            .map(|(_, values)| values.as_slice())
    }
}

/// Parses `RelayResponse.Payload` into the backend's answer.
pub fn parse_pokt_http_response(payload: &[u8]) -> Result<PoktHttpResponse, VerifyError> {
    let mut status_code = 0u32;
    let mut headers = Vec::new();
    let mut body = Vec::new();

    for record in parse_records(payload)? {
        match record.number {
            http_field::STATUS_CODE if record.wire_type == 0 => {
                let raw = record.varint()?;
                status_code = u32::try_from(raw).map_err(|_| VerifyError::StatusCodeRange(raw))?;
            }
            // Repeated, so no `unique` here: one record per header key.
            http_field::HEADER if record.wire_type == 2 => {
                headers.push(parse_header_entry(record.value)?);
            }
            http_field::BODY_BZ if record.wire_type == 2 => body = record.value.to_vec(),
            // Anything else is a field added after this example was written.
            // Skipping it is correct: nothing here is signed, so an unknown
            // field cannot invalidate anything.
            _ => {}
        }
    }

    Ok(PoktHttpResponse {
        status_code,
        headers,
        body,
    })
}

/// One `map<string, Header>` entry: `{1: key, 2: Header{1: key, 2: [values]}}`.
///
/// The map key and `Header.key` are the same string, written twice. The map
/// key is the one that matters — it is what a protobuf map lookup uses — so
/// that is the one read here.
fn parse_header_entry(entry: &[u8]) -> Result<(String, Vec<String>), VerifyError> {
    let records = parse_records(entry)?;
    let key = match unique(&records, http_field::MAP_KEY)? {
        Some(record) => record.utf8()?,
        None => String::new(),
    };

    let mut values = Vec::new();
    if let Some(header) = unique(&records, http_field::MAP_VALUE)? {
        for record in parse_records(length_delimited(header)?)? {
            match record.number {
                // The second copy of the key. Skipped on purpose; see above.
                http_field::HEADER_KEY => {}
                http_field::HEADER_VALUES => values.push(record.utf8()?),
                _ => {}
            }
        }
    }

    Ok((key, values))
}

// ---------------------------------------------------------------------------
// The whole return trip
// ---------------------------------------------------------------------------

/// Verifies everything a caller can verify, and returns the backend's answer.
///
/// This is the function to copy. The order is not cosmetic:
///
/// 1. the supplier operator signature, over the filtered bytes;
/// 2. `sha256(payload) == payload_hash` — **without this the signature check
///    means nothing**, because the payload is not in the signed bytes;
/// 3. the receipt, if one was asked for and returned, binding the response to
///    the request that produced it;
/// 4. only then, parse the payload.
///
/// `receipt_header` is the `Pocket-Relay-Receipt` value, or `None`. A relayer
/// returns one only when the request carried `Pocket-Sign-Receipt: true`, so
/// `None` is normal. If you did ask for one, treat its absence as a failure at
/// the call site: this function cannot tell the difference between "not
/// requested" and "dropped".
///
/// ```no_run
/// # use pocket_relay_signing::verify::open_relay_response;
/// # fn demo(bytes: &[u8], supplier_pubkey: &[u8], request_signature: &[u8], header: Option<&str>)
/// # -> Result<(), Box<dyn std::error::Error>> {
/// let http = open_relay_response(bytes, supplier_pubkey, request_signature, header)?;
/// assert_eq!(http.status_code, 200);
/// println!("{}", String::from_utf8_lossy(&http.body));
/// # Ok(()) }
/// ```
pub fn open_relay_response(
    relay_response: &[u8],
    supplier_pubkey: &[u8],
    request_signature: &[u8],
    receipt_header: Option<&str>,
) -> Result<PoktHttpResponse, VerifyError> {
    verify_response_signature(relay_response, supplier_pubkey)?;

    let payload = payload(relay_response)?;
    let signed_hash = payload_hash(relay_response)?;
    // Plain comparison: both sides are public. Constant time buys nothing here
    // and the habit of reaching for it hides where it IS needed.
    if sha256(payload) != signed_hash {
        return Err(VerifyError::PayloadHashMismatch);
    }

    if let Some(header_value) = receipt_header {
        verify_receipt(
            header_value,
            request_signature,
            signed_hash,
            supplier_pubkey,
        )?;
    }

    parse_pokt_http_response(payload)
}

fn sha256(bytes: &[u8]) -> [u8; HASH_LEN] {
    Sha256::digest(bytes).into()
}

// ---------------------------------------------------------------------------
// Tests
//
// The response half of `oracle response-vectors` is deterministic (fixed
// supplier key, RFC 6979 nonces) and is pinned below. The receipt half is NOT:
// the request signature it binds is a real bLSAG signature and bLSAG is
// randomised, so a fresh oracle run emits different — equally valid — receipt
// bytes. What is pinned here is one captured run. `src/main.rs --response`
// covers the live oracle, in both directions.
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use k256::elliptic_curve::bigint::Encoding;
    use k256::elliptic_curve::ops::Reduce;
    use k256::elliptic_curve::PrimeField;
    use k256::{Scalar, U256};

    const SUPPLIER_PUB_HEX: &str =
        "027bab07fac0ca93a67730cb78a32bf5fac835c1e5aed5d1eb42c2e6f22d244c18";

    const RELAY_RESPONSE_HEX: &str = "0ac5010a80010a2b706f6b74316d7271743566377168387578733237636a6d397437763965373461397676646e71356a766134120b6578616d706c652d7376631a40303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030312064286e12403da5c816b726e16b3aa033f67f375b01f0e1f652d3b945eeef1bcc856da85b1c5c5b537296b8a33b33de787a86608c32b4325046d24b3c5023bb30a7938ab9ad126108c80112300a0c436f6e74656e742d5479706512200a0c436f6e74656e742d5479706512106170706c69636174696f6e2f6a736f6e1a2a7b226a736f6e727063223a22322e30222c226964223a312c22726573756c74223a22307831323334227d2220d96c9f7eb71290eecc3bd9c29bcdc862efc31dad3e9aa1f265e6e60b6d2714cd";

    const SIGNABLE_BYTES_HEX: &str = "0a83010a80010a2b706f6b74316d7271743566377168387578733237636a6d397437763965373461397676646e71356a766134120b6578616d706c652d7376631a40303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030303030312064286e2220d96c9f7eb71290eecc3bd9c29bcdc862efc31dad3e9aa1f265e6e60b6d2714cd";

    const RESPONSE_SIGNATURE_HEX: &str = "3da5c816b726e16b3aa033f67f375b01f0e1f652d3b945eeef1bcc856da85b1c5c5b537296b8a33b33de787a86608c32b4325046d24b3c5023bb30a7938ab9ad";

    const PAYLOAD_HEX: &str = "08c80112300a0c436f6e74656e742d5479706512200a0c436f6e74656e742d5479706512106170706c69636174696f6e2f6a736f6e1a2a7b226a736f6e727063223a22322e30222c226964223a312c22726573756c74223a22307831323334227d";

    const PAYLOAD_HASH_HEX: &str =
        "d96c9f7eb71290eecc3bd9c29bcdc862efc31dad3e9aa1f265e6e60b6d2714cd";

    const BODY_TEXT: &str = r#"{"jsonrpc":"2.0","id":1,"result":"0x1234"}"#;

    // -- one captured run of the randomised receipt half ------------------

    const REQUEST_SIGNATURE_HEX: &str = "000000029591f75617830ac50fda3f50d108dcd787d6f17144ea05db53d53eec3787537502c82a2efdba0348c63e87377a0fb0d882a4fc696744aec6b6991bcbe1b23645361d60f526ef278d0fe8349a8ec0d7a81f6f71b47c4d76ef7128e7f84f5afbf8540397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc5fdcc8ac10e096a0a13e870c4a1a919dd7710223969368385b1ba09b5b4b7679d02bbbf99abdcddac27350bca272d7146187c091aacfc1c6f90819c9b6daf4fe846";

    const RECEIPT_DIGEST_HEX: &str =
        "6a8d2461f0f928fb12eeec7faa6aeea15bce33eb8bcff4d0840c41ebefac6e0a";

    const RECEIPT_HEADER_VALUE: &str = "v1.c8041713018ad6dc8fb62ae150c8c7336975a110d85d259758831720b764c5621bb0bf46e77d9949fd057c1f9542c8ebf9ead3a3082f221781837d35d5f4dedb";

    /// A different response's payload hash.
    const OTHER_PAYLOAD_HASH_HEX: &str =
        "faff3e43f976fe1254d477984ae2fb02bf74429d578310815b232ced64f6280e";

    /// A different relay's request signature.
    const OTHER_REQUEST_SIGNATURE_HEX: &str = "000000024799988b8b02ac66d35721a428f7d00780cb9c74c9f785d6fd0a0ce5b1ce31fa02c82a2efdba0348c63e87377a0fb0d882a4fc696744aec6b6991bcbe1b23645363c9fffad48ecc2fd6d06ee1fb39155396df18c219515b65ffd2261eb8aa391eb0397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc59e8f79ce0f1698c004b3ca805dcfb6f19260efa94f7f20900e70363770e9b98202bbbf99abdcddac27350bca272d7146187c091aacfc1c6f90819c9b6daf4fe846";

    fn unhex(s: &str) -> Vec<u8> {
        hex::decode(s).expect("test vector must be hex")
    }

    fn response() -> Vec<u8> {
        unhex(RELAY_RESPONSE_HEX)
    }

    fn pubkey() -> Vec<u8> {
        unhex(SUPPLIER_PUB_HEX)
    }

    // -- the filter -------------------------------------------------------

    /// The load-bearing one. Everything downstream hashes these bytes, so if
    /// they are off by a byte nothing else in this file can pass.
    #[test]
    fn signable_bytes_match_the_oracle() {
        assert_eq!(
            hex::encode(signable_bytes(&response()).unwrap()),
            SIGNABLE_BYTES_HEX,
        );
    }

    /// The filter must keep `payload_hash`: it is what stands in for the body
    /// in the signed message. Dropping it would still produce a signature that
    /// verifies against *some* bytes, just not the supplier's.
    #[test]
    fn signable_bytes_keep_the_payload_hash() {
        let signable = signable_bytes(&response()).unwrap();
        assert!(signable
            .windows(HASH_LEN)
            .any(|w| w == unhex(PAYLOAD_HASH_HEX).as_slice()));
    }

    #[test]
    fn signable_bytes_drop_the_payload_and_the_signature() {
        let signable = signable_bytes(&response()).unwrap();
        for (name, dropped) in [
            ("payload", unhex(PAYLOAD_HEX)),
            ("signature", unhex(RESPONSE_SIGNATURE_HEX)),
        ] {
            assert!(
                !signable
                    .windows(dropped.len())
                    .any(|w| w == dropped.as_slice()),
                "{name} must not survive into the signable bytes",
            );
        }
    }

    /// Backwards compatibility: a supplier older than poktroll v0.1.25 sends no
    /// `payload_hash` and signs the payload itself. Dropping the payload
    /// unconditionally would reject every one of those responses.
    #[test]
    fn payload_is_kept_when_there_is_no_payload_hash() {
        // The `payload_hash` record is the last 34 bytes: tag 0x22, length
        // 0x20, 32 bytes of hash. Sliced by hand so this test does not depend
        // on the parser it is testing.
        let full = response();
        let without_hash = &full[..full.len() - (2 + HASH_LEN)];
        assert_eq!(&full[full.len() - (2 + HASH_LEN)..][..2], &[0x22, 0x20]);

        // Expected: the pinned signable bytes with their trailing payload_hash
        // record replaced by the payload record (tag 0x12, length 97).
        let signable_with_hash = unhex(SIGNABLE_BYTES_HEX);
        let payload = unhex(PAYLOAD_HEX);
        let mut expected = signable_with_hash[..signable_with_hash.len() - (2 + HASH_LEN)].to_vec();
        expected.push(0x12);
        expected.push(u8::try_from(payload.len()).unwrap());
        expected.extend_from_slice(&payload);

        assert_eq!(signable_bytes(without_hash).unwrap(), expected);
    }

    #[test]
    fn rejects_a_truncated_response() {
        let full = response();
        let err = signable_bytes(&full[..full.len() - 4]).unwrap_err();
        assert!(
            matches!(err, VerifyError::Truncated { .. }),
            "got {err:?}, want Truncated",
        );
    }

    /// `meta` must be a nested message. Arriving as a varint, its bytes would
    /// otherwise be parsed as one and whatever fell out reported as a session
    /// header.
    #[test]
    fn rejects_a_meta_that_is_not_a_message() {
        // Field 1, wire type 0 (varint), value 1 — then the real payload_hash
        // record so the rest of the message is well formed.
        let mut wrong = vec![0x08, 0x01];
        let full = response();
        wrong.extend_from_slice(&full[full.len() - (2 + HASH_LEN)..]);

        assert_eq!(
            signable_bytes(&wrong),
            Err(VerifyError::UnsupportedWireType {
                number: field::META,
                wire_type: 0,
            }),
        );
    }

    #[test]
    fn rejects_a_duplicated_meta() {
        let mut doubled = response();
        let meta_len = 3 + 197; // tag, 2-byte length varint, 197 bytes of meta
        let meta = doubled[..meta_len].to_vec();
        doubled.extend_from_slice(&meta);
        assert_eq!(
            signable_bytes(&doubled),
            Err(VerifyError::DuplicateField(field::META)),
        );
    }

    // -- the response signature -------------------------------------------

    #[test]
    fn response_signature_verifies() {
        verify_response_signature(&response(), &pubkey()).expect("the oracle signed this");
    }

    #[test]
    fn response_signature_fails_under_a_different_key() {
        // The app pubkey from `oracle vectors` — a real key, wrong signer.
        let other = unhex("0397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc5");
        assert_eq!(
            verify_response_signature(&response(), &other),
            Err(VerifyError::BadSignature),
        );
    }

    /// `payload_hash` is inside the signed bytes, so changing it must break the
    /// signature. This is what makes the hash worth comparing against at all.
    #[test]
    fn tampering_with_the_payload_hash_breaks_the_signature() {
        let mut tampered = response();
        let last = tampered.len() - 1;
        tampered[last] ^= 0x01;
        assert_eq!(
            verify_response_signature(&tampered, &pubkey()),
            Err(VerifyError::BadSignature),
        );
    }

    /// Trap 1, as a test. The payload is NOT in the signed bytes, so swapping
    /// it leaves the signature valid — only the hash comparison catches it.
    #[test]
    fn a_swapped_payload_keeps_a_valid_signature_and_is_caught_by_the_hash() {
        let mut tampered = response();
        let payload = unhex(PAYLOAD_HEX);
        let at = tampered
            .windows(payload.len())
            .position(|w| w == payload.as_slice())
            .expect("the payload is in the response");
        tampered[at + payload.len() - 2] ^= 0x01; // one byte of the JSON body

        verify_response_signature(&tampered, &pubkey())
            .expect("the payload is not covered by the signature");
        assert_eq!(
            open_relay_response(&tampered, &pubkey(), &unhex(REQUEST_SIGNATURE_HEX), None),
            Err(VerifyError::PayloadHashMismatch),
        );
    }

    /// ECDSA is malleable: `(R, N-S)` verifies wherever `(R, S)` does. Cosmos
    /// rejects the high half, so this must too — otherwise a verifier accepts
    /// responses the network treats as unsigned.
    #[test]
    fn a_high_s_signature_is_rejected() {
        let signature = unhex(RESPONSE_SIGNATURE_HEX);
        let s_bytes: [u8; 32] = signature[32..].try_into().unwrap();

        let s = <Scalar as Reduce<U256>>::reduce(&U256::from_be_bytes(s_bytes.into()));
        assert!(
            !bool::from(s.is_high()),
            "the oracle's own signature must be low-S",
        );

        // N - s is the other valid S for this signature.
        let flipped = -s;
        assert!(bool::from(flipped.is_high()), "N - s must be the high half");

        let mut high = signature[..32].to_vec();
        high.extend_from_slice(&flipped.to_repr());

        let signable_hash = signable_bytes_hash(&response()).unwrap();
        assert_eq!(
            verify_cosmos_secp256k1(&signable_hash, &high, &pubkey()),
            Err(VerifyError::HighS),
        );
    }

    #[test]
    fn rejects_malformed_keys_and_signatures() {
        let signable_hash = signable_bytes_hash(&response()).unwrap();
        let signature = unhex(RESPONSE_SIGNATURE_HEX);

        assert_eq!(
            verify_cosmos_secp256k1(&signable_hash, &signature[..63], &pubkey()),
            Err(VerifyError::SignatureLength(63)),
        );
        assert_eq!(
            verify_cosmos_secp256k1(&signable_hash, &signature, &pubkey()[..32]),
            Err(VerifyError::PublicKeyLength(32)),
        );
        // 33 bytes, but 0x04 is the uncompressed prefix and the rest is not a point.
        let mut not_a_point = vec![0x04u8];
        not_a_point.extend_from_slice(&[0x11; 32]);
        assert_eq!(
            verify_cosmos_secp256k1(&signable_hash, &signature, &not_a_point),
            Err(VerifyError::PublicKeyInvalid),
        );
        assert_eq!(
            verify_cosmos_secp256k1(&signable_hash, &[0u8; 64], &pubkey()),
            Err(VerifyError::SignatureMalformed),
        );
    }

    // -- the receipt ------------------------------------------------------

    #[test]
    fn receipt_digest_matches_the_oracle() {
        let digest =
            receipt_digest(&unhex(REQUEST_SIGNATURE_HEX), &unhex(PAYLOAD_HASH_HEX)).unwrap();
        assert_eq!(hex::encode(digest), RECEIPT_DIGEST_HEX);
    }

    #[test]
    fn receipt_verifies() {
        verify_receipt(
            RECEIPT_HEADER_VALUE,
            &unhex(REQUEST_SIGNATURE_HEX),
            &unhex(PAYLOAD_HASH_HEX),
            &pubkey(),
        )
        .expect("the oracle signed this receipt");
    }

    /// Negative control 1: the receipt must not verify against a different
    /// response. Without this, a relayer could answer every request with the
    /// same receipt.
    #[test]
    fn receipt_fails_against_another_responses_payload_hash() {
        assert_eq!(
            verify_receipt(
                RECEIPT_HEADER_VALUE,
                &unhex(REQUEST_SIGNATURE_HEX),
                &unhex(OTHER_PAYLOAD_HASH_HEX),
                &pubkey(),
            ),
            Err(VerifyError::BadSignature),
        );
    }

    /// Negative control 2: the receipt must not verify against a different
    /// request. This is the binding the receipt exists for — a receipt that
    /// passed here would prove nothing about which relay it answers.
    #[test]
    fn receipt_fails_against_another_requests_signature() {
        assert_eq!(
            verify_receipt(
                RECEIPT_HEADER_VALUE,
                &unhex(OTHER_REQUEST_SIGNATURE_HEX),
                &unhex(PAYLOAD_HASH_HEX),
                &pubkey(),
            ),
            Err(VerifyError::BadSignature),
        );
    }

    #[test]
    fn receipt_digest_needs_a_full_length_payload_hash() {
        // A short hash would leave the request signature's extent ambiguous.
        assert_eq!(
            receipt_digest(
                &unhex(REQUEST_SIGNATURE_HEX),
                &unhex(PAYLOAD_HASH_HEX)[..31]
            ),
            Err(VerifyError::PayloadHashLength(31)),
        );
        assert_eq!(
            receipt_digest(&[], &unhex(PAYLOAD_HASH_HEX)),
            Err(VerifyError::ReceiptEmptyRequestSignature),
        );
    }

    #[test]
    fn receipt_header_parsing() {
        let expected = parse_receipt_header(RECEIPT_HEADER_VALUE).unwrap();
        assert_eq!(expected.len(), SIGNATURE_LEN);

        // Either case, same bytes.
        let upper = format!(
            "{RECEIPT_HEADER_PREFIX}{}",
            RECEIPT_HEADER_VALUE[RECEIPT_HEADER_PREFIX.len()..].to_uppercase()
        );
        assert_eq!(parse_receipt_header(&upper).unwrap(), expected);

        assert_eq!(
            parse_receipt_header(&RECEIPT_HEADER_VALUE[3..]),
            Err(VerifyError::ReceiptVersion),
        );
        assert_eq!(
            parse_receipt_header(&RECEIPT_HEADER_VALUE.replace("v1.", "v2.")),
            Err(VerifyError::ReceiptVersion),
        );
        assert_eq!(
            parse_receipt_header(&RECEIPT_HEADER_VALUE[..RECEIPT_HEADER_VALUE.len() - 1]),
            Err(VerifyError::ReceiptLength { got: 127 }),
        );
        assert_eq!(
            parse_receipt_header(&format!("{RECEIPT_HEADER_PREFIX}{}", "z".repeat(128))),
            Err(VerifyError::ReceiptNotHex),
        );
    }

    // -- the payload ------------------------------------------------------

    #[test]
    fn inner_pokt_http_response_parses() {
        let http = parse_pokt_http_response(&unhex(PAYLOAD_HEX)).unwrap();

        assert_eq!(http.status_code, 200);
        assert_eq!(String::from_utf8(http.body.clone()).unwrap(), BODY_TEXT);
        assert_eq!(
            http.header("content-type"),
            Some(["application/json".to_owned()].as_slice()),
            "header lookup must be case-insensitive",
        );
        assert_eq!(http.headers.len(), 1);
    }

    #[test]
    fn payload_hash_is_sha256_of_the_payload() {
        let payload = unhex(PAYLOAD_HEX);
        assert_eq!(hex::encode(sha256(&payload)), PAYLOAD_HASH_HEX);
        assert_eq!(
            hex::encode(payload_hash(&response()).unwrap()),
            PAYLOAD_HASH_HEX
        );
    }

    // -- the whole trip ---------------------------------------------------

    #[test]
    fn open_relay_response_accepts_the_oracles_response_and_receipt() {
        let http = open_relay_response(
            &response(),
            &pubkey(),
            &unhex(REQUEST_SIGNATURE_HEX),
            Some(RECEIPT_HEADER_VALUE),
        )
        .expect("everything in the vectors must check out");

        assert_eq!(http.status_code, 200);
        assert_eq!(String::from_utf8(http.body).unwrap(), BODY_TEXT);
    }

    #[test]
    fn open_relay_response_without_a_receipt_still_checks_the_payload() {
        let http = open_relay_response(&response(), &pubkey(), &[], None).unwrap();
        assert_eq!(http.status_code, 200);
    }

    #[test]
    fn open_relay_response_rejects_a_receipt_from_another_relay() {
        assert_eq!(
            open_relay_response(
                &response(),
                &pubkey(),
                &unhex(OTHER_REQUEST_SIGNATURE_HEX),
                Some(RECEIPT_HEADER_VALUE),
            ),
            Err(VerifyError::BadSignature),
        );
    }
}
