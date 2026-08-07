#!/usr/bin/env node
/**
 * Pocket Network relay response — a verifier for Node.js (the return trip).
 *
 * Signing a relay gets it accepted. This file is the other half: reading what
 * comes back and proving it is an answer to the request you sent, from the
 * supplier you addressed. Two independent artefacts do that:
 *
 *   1. RelayResponse.Meta.SupplierOperatorSignature — the supplier signs the
 *      response. Anyone can check it against the supplier's on-chain key.
 *   2. The `Pocket-Relay-Receipt` header (optional; ask for it with
 *      `Pocket-Sign-Receipt: true`) — the supplier signs
 *      request-signature || response-payload-hash together, which is what binds
 *      THIS response to THAT request. The response signature alone does not:
 *      it says "a supplier signed this response", not "in reply to you".
 *
 * THE ENTRY POINT IS openRelayResponse(). It runs all four checks in order —
 * signature, payload binding, receipt, and only then the payload — and hands
 * back the backend's answer. It is the same function, under the same name and
 * with the same scope, in the Python and Rust ports next door. Everything else
 * exported here is a piece of it, so a port can bisect against the oracle one
 * layer at a time; a name ending in `Only` is a piece, not a verification.
 *
 * Four things in here are surprising, and all four are load-bearing:
 *
 *   1. The signed bytes are obtained by FILTERING the protobuf you received,
 *      never by re-encoding it.
 *   2. Cosmos secp256k1 hashes AGAIN internally, so the ECDSA message hash is
 *      sha256(sha256(signable bytes)). Miss it and nothing ever verifies.
 *   3. Low-S is mandatory. Cosmos rejects S > N/2; so must you.
 *   4. The signature covers the payload HASH, not the payload. If you do not
 *      check sha256(payload) yourself, the body you read is unauthenticated.
 *
 * Each is commented where it happens. If you are porting this to another
 * language, those comments are the whole point of the file.
 *
 * Usage:
 *   go build -o ./oracle-bin ../oracle && ./oracle-bin response-vectors | node verify.mjs
 *
 * With no stdin it runs the oracle itself (ORACLE=/path/to/oracle to override).
 *
 * No protobuf library. RelayResponse has four fields and POKTHTTPResponse has
 * three; the ~80 lines of wire parsing below are the entire dependency you would
 * otherwise take on, and they show that there is no magic in the format.
 */

import { readFileSync, realpathSync } from 'node:fs';

import { secp256k1 } from '@noble/curves/secp256k1.js';
import { bytesToHex, concatBytes, hexToBytes } from '@noble/curves/utils.js';
import { sha256 } from '@noble/hashes/sha2.js';

// ---------------------------------------------------------------------------
// Protocol constants. Restated, not imported.
// ---------------------------------------------------------------------------
// A verifier that shares a constant with the producer cannot catch the producer
// changing it. These MUST match relayer/receipt.go; the harness proves they do
// by checking them against the oracle's vectors.

/** Response header carrying the receipt. Value is `v1.<128 hex chars>`. */
export const RECEIPT_HEADER = 'Pocket-Relay-Receipt';

/** Request header that asks for one. Absent means no. */
export const RECEIPT_REQUEST_HEADER = 'Pocket-Sign-Receipt';

/**
 * Domain tag for the receipt digest: the 22 bytes of "POKT-RELAY-RECEIPT-v1\0",
 * TRAILING NUL INCLUDED. It separates this signing context from every other
 * thing the supplier key signs, so a signature harvested from elsewhere cannot
 * be replayed as a receipt. Dropping the NUL gives 21 bytes and a digest that
 * never verifies — and the failure looks exactly like a wrong key.
 */
export const RECEIPT_DOMAIN_TAG = new TextEncoder().encode('POKT-RELAY-RECEIPT-v1\0');

/** RelayResponse field numbers (poktroll x/service/types/relay.proto). */
const F_META = 1;
const F_PAYLOAD = 2;
const F_RELAY_MINER_ERROR = 3;
const F_PAYLOAD_HASH = 4;

/** RelayResponseMetadata field numbers. */
const F_META_SESSION_HEADER = 1;
const F_META_SUPPLIER_SIGNATURE = 2;

/** SessionHeader field numbers (poktroll x/session/types/types.proto). */
const F_HDR_SERVICE_ID = 2;

/** POKTHTTPResponse field numbers (shannon-sdk types). */
const F_HTTP_STATUS = 1;
const F_HTTP_HEADER = 2;
const F_HTTP_BODY = 3;

/** Order of the secp256k1 group; S must be <= (N-1)/2, i.e. below the halfway point. */
const N = secp256k1.Point.Fn.ORDER;
const HALF_N = N >> 1n;

// ---------------------------------------------------------------------------
// Protobuf wire format, the 80 lines of it we need.
// ---------------------------------------------------------------------------

/**
 * Read a base-128 varint.
 *
 * Seven bits per byte, little-endian groups, high bit set means "another byte
 * follows". Capped at 10 bytes (the maximum for a 64-bit value) so a malicious
 * run of 0x80 cannot spin here.
 *
 * @param {Uint8Array} bytes
 * @param {number} pos offset to read from
 * @returns {{value: number, next: number}} value, and the offset after it
 */
export function readVarint(bytes, pos) {
  let value = 0n;
  let shift = 0n;

  for (let i = 0; i < 10; i++) {
    if (pos >= bytes.length) throw new Error(`truncated varint at offset ${pos}`);
    const b = bytes[pos++];
    value |= BigInt(b & 0x7f) << shift;
    if ((b & 0x80) === 0) {
      // Lengths and field numbers are the only varints this file reads, and
      // both are bounded by the buffer. Number is exact to 2^53, so a value
      // beyond that is corrupt input, not a big message.
      if (value > BigInt(Number.MAX_SAFE_INTEGER)) {
        throw new Error(`varint at offset ${pos} does not fit in a JS number`);
      }
      return { value: Number(value), next: pos };
    }
    shift += 7n;
  }
  throw new Error(`varint at offset ${pos} is longer than 10 bytes`);
}

/**
 * Encode a non-negative integer as a varint, minimally.
 *
 * Only used to rewrite ONE length prefix (the meta field's, after the signature
 * is filtered out of it). "Minimally" matters: protobuf permits 0x80 0x00 as a
 * padded zero, gogoproto never emits it, and Go's re-marshal would not either —
 * so the minimal form is the one that reproduces the signed bytes.
 *
 * @param {number} n
 * @returns {Uint8Array}
 */
export function encodeVarint(n) {
  if (!Number.isInteger(n) || n < 0) throw new Error(`encodeVarint needs a non-negative integer, got ${n}`);
  const out = [];
  let v = n;
  do {
    const b = v & 0x7f;
    v = Math.floor(v / 128);
    out.push(v > 0 ? b | 0x80 : b);
  } while (v > 0);
  return Uint8Array.from(out);
}

/**
 * Split a protobuf message into its top-level records, without decoding them.
 *
 * Each record keeps its exact byte extent in the ORIGINAL buffer. That is the
 * whole trick behind filterSignableBytes: a record you keep is copied verbatim,
 * bit for bit, so you never have to reproduce the encoder's choices.
 *
 * @param {Uint8Array} bytes
 * @returns {Array<{field: number, wireType: number, start: number, tagEnd: number, end: number, valueStart: number, valueEnd: number}>}
 */
export function splitProtoFields(bytes) {
  const records = [];
  let pos = 0;

  while (pos < bytes.length) {
    const start = pos;
    const { value: tag, next: tagEnd } = readVarint(bytes, pos);
    pos = tagEnd;

    const field = tag >>> 3;
    const wireType = tag & 0x7;
    if (field === 0) throw new Error(`field number 0 at offset ${start} is not legal protobuf`);

    let valueStart = pos;
    let valueEnd;
    switch (wireType) {
      case 0: {
        // varint
        valueEnd = readVarint(bytes, pos).next;
        break;
      }
      case 1: // 64-bit
        valueEnd = pos + 8;
        break;
      case 2: {
        // length-delimited
        const len = readVarint(bytes, pos);
        valueStart = len.next;
        valueEnd = len.next + len.value;
        break;
      }
      case 5: // 32-bit
        valueEnd = pos + 4;
        break;
      default:
        // 3 and 4 are the deprecated groups; nothing in these messages uses
        // them, and guessing would let us walk off into garbage silently.
        throw new Error(`unsupported wire type ${wireType} for field ${field} at offset ${start}`);
    }
    if (valueEnd > bytes.length) {
      throw new Error(`field ${field} at offset ${start} runs past the end of the buffer`);
    }

    records.push({ field, wireType, start, tagEnd, end: valueEnd, valueStart, valueEnd });
    pos = valueEnd;
  }
  return records;
}

/** The value bytes of a record, as a subarray of the original buffer. */
const valueOf = (bytes, rec) => bytes.subarray(rec.valueStart, rec.valueEnd);

/** The whole record — tag, length and value — as a subarray of the original buffer. */
const recordOf = (bytes, rec) => bytes.subarray(rec.start, rec.end);

// ---------------------------------------------------------------------------
// The signable bytes.
// ---------------------------------------------------------------------------

/**
 * Recover the exact bytes the supplier signed, by FILTERING what you received.
 *
 * The supplier signs the RelayResponse with two things removed: its own
 * signature (field 2 inside meta — obviously, it does not exist yet) and the
 * payload (top-level field 2), because payload_hash already commits to it and
 * dropping it keeps the on-chain proof small. Everything else stays, including
 * payload_hash and relay_miner_error.
 *
 * FILTER, DO NOT RE-ENCODE. Parsing into your own structs and marshalling them
 * back gives bytes that are equal only if your encoder makes every choice the
 * producer's did — field order, varint minimality, default-value omission — and
 * it silently DROPS any field your build does not know about. The day the
 * relayer adds field 5, a re-encoding verifier rejects every response on the
 * network and a filtering one keeps working. So: walk the records, skip two,
 * copy the rest verbatim.
 *
 * Exactly one byte range is rewritten: meta's length prefix, which must shrink
 * by the whole signature record (its tag, its length varint and its 64 bytes —
 * 66 in total for a normal response). That is unavoidable, and it is why
 * encodeVarint has to be minimal.
 *
 * The one input this cannot handle is a non-canonically encoded response (fields
 * out of order, padded varints). Go's verifier unmarshals and re-marshals, so it
 * would normalise those and we would not; gogoproto never produces them, and a
 * mismatch shows up as a failed signature rather than a wrong accept.
 *
 * @param {Uint8Array} relayResponseBz the RelayResponse protobuf as received
 * @returns {Uint8Array} the bytes to hash and verify against
 */
export function filterSignableBytes(relayResponseBz) {
  const records = splitProtoFields(relayResponseBz);

  // The payload is dropped ONLY when payload_hash is present. This is not
  // defensive coding, it is the rule poktroll implements: payload_hash arrived
  // in v0.1.25, and responses signed before it covered the whole payload. A
  // verifier that always drops the payload gets every pre-v0.1.25 response
  // wrong. See RelayResponse.GetSignableBytesHash.
  const hasPayloadHash = records.some((r) => r.field === F_PAYLOAD_HASH);

  const parts = [];
  for (const rec of records) {
    if (rec.field === F_PAYLOAD && hasPayloadHash) continue;

    if (rec.field === F_META && rec.wireType === 2) {
      const meta = valueOf(relayResponseBz, rec);
      const kept = splitProtoFields(meta)
        .filter((m) => m.field !== F_META_SUPPLIER_SIGNATURE)
        .map((m) => recordOf(meta, m));
      const inner = concatBytes(...kept);

      // Tag verbatim, length recomputed, inner records verbatim.
      parts.push(relayResponseBz.subarray(rec.start, rec.tagEnd), encodeVarint(inner.length), inner);
      continue;
    }

    parts.push(recordOf(relayResponseBz, rec));
  }
  return concatBytes(...parts);
}

// ---------------------------------------------------------------------------
// RelayResponse.
// ---------------------------------------------------------------------------

/**
 * Pull the four fields a caller needs out of a RelayResponse.
 *
 * Returns raw bytes, not decoded structures: this file's job is to check
 * signatures, and every byte it hands back is a byte it read out of the buffer
 * you gave it.
 *
 * @param {Uint8Array} relayResponseBz
 * @returns {{
 *   payload: Uint8Array|null,
 *   payloadHash: Uint8Array|null,
 *   supplierSignature: Uint8Array|null,
 *   sessionHeader: Uint8Array|null,
 *   relayMinerError: Uint8Array|null,
 * }}
 */
export function parseRelayResponse(relayResponseBz) {
  const out = {
    payload: null,
    payloadHash: null,
    supplierSignature: null,
    sessionHeader: null,
    relayMinerError: null,
  };

  for (const rec of splitProtoFields(relayResponseBz)) {
    switch (rec.field) {
      case F_META: {
        const meta = valueOf(relayResponseBz, rec);
        for (const m of splitProtoFields(meta)) {
          if (m.field === F_META_SESSION_HEADER) out.sessionHeader = valueOf(meta, m);
          if (m.field === F_META_SUPPLIER_SIGNATURE) out.supplierSignature = valueOf(meta, m);
        }
        break;
      }
      case F_PAYLOAD:
        out.payload = valueOf(relayResponseBz, rec);
        break;
      case F_RELAY_MINER_ERROR:
        out.relayMinerError = valueOf(relayResponseBz, rec);
        break;
      case F_PAYLOAD_HASH:
        out.payloadHash = valueOf(relayResponseBz, rec);
        break;
      default:
        // Unknown fields are not an error — a newer relayer may send them, and
        // filterSignableBytes preserves them so the signature still checks out.
        break;
    }
  }
  return out;
}

/**
 * Verify a cosmos secp256k1 signature over a 32-byte digest.
 *
 * TWO THINGS THAT COST PEOPLE DAYS, both of them here:
 *
 * 1. THE DOUBLE HASH. cosmos-sdk's secp256k1 PrivKey.Sign(msg) runs
 *    sha256(msg) before ECDSA, and VerifySignature does the same. Everything in
 *    Pocket already hands it a 32-byte digest, so the value actually signed is
 *    sha256(digest) — a hash of a hash. @noble/curves has its own `prehash`
 *    option that defaults to TRUE, i.e. it would sha256 for you; relying on
 *    that default happens to be right here but is invisible to the reader and
 *    breaks the moment the default changes. We hash explicitly and pass
 *    prehash:false, which means "this IS the ECDSA message hash".
 *
 * 2. LOW-S IS MANDATORY. Every ECDSA signature (r, s) has a twin (r, N-s) that
 *    is equally valid mathematically. Cosmos rejects the high one, so a
 *    verifier that accepts it accepts signatures the chain never would — and,
 *    worse, gives an attacker a second byte-string for the same authorised
 *    message, which breaks anything keyed on the signature bytes.
 *    @noble/curves defaults to lowS:true; again, we say it out loud, and we
 *    check S ourselves first so the failure reports WHY rather than just false.
 *
 * @param {Uint8Array} signature 64 bytes, R||S
 * @param {Uint8Array} digest32 the 32 bytes cosmos was asked to sign
 * @param {Uint8Array} publicKey33 compressed SEC1 secp256k1 key
 * @returns {{valid: boolean, reason?: string, ecdsaMessageHash: Uint8Array}}
 */
export function verifyCosmosSecp256k1(signature, digest32, publicKey33) {
  const ecdsaMessageHash = sha256(digest32); // (1) the double hash

  if (!(signature instanceof Uint8Array) || signature.length !== 64) {
    return { valid: false, reason: `signature must be 64 bytes R||S, got ${signature?.length ?? 0}`, ecdsaMessageHash };
  }
  if (!(digest32 instanceof Uint8Array) || digest32.length !== 32) {
    return { valid: false, reason: `digest must be 32 bytes, got ${digest32?.length ?? 0}`, ecdsaMessageHash };
  }
  if (!(publicKey33 instanceof Uint8Array) || publicKey33.length !== 33) {
    return {
      valid: false,
      reason:
        `public key must be a 33-byte compressed SEC1 key, got ${publicKey33?.length ?? 0}` +
        (publicKey33?.length === 65 ? ' (that is an UNCOMPRESSED key; compress it first)' : ''),
      ecdsaMessageHash,
    };
  }

  let sig;
  try {
    sig = secp256k1.Signature.fromBytes(signature); // 64-byte compact R||S
  } catch (err) {
    return { valid: false, reason: `signature is not a valid (r, s) pair: ${err.message}`, ecdsaMessageHash };
  }
  if (sig.s > HALF_N) {
    // (2) low-S, checked explicitly so the reason is legible. noble would also
    // refuse below, silently, and you would spend an afternoon on it.
    return { valid: false, reason: 'signature has high S; cosmos secp256k1 requires the low-S form', ecdsaMessageHash };
  }

  let ok;
  try {
    ok = secp256k1.verify(signature, ecdsaMessageHash, publicKey33, {
      prehash: false, // we already hashed: this IS the message hash
      lowS: true, // belt and braces with the explicit check above
    });
  } catch (err) {
    return { valid: false, reason: `verify threw: ${err.message}`, ecdsaMessageHash };
  }
  return ok ? { valid: true, ecdsaMessageHash } : { valid: false, reason: 'signature does not match this key', ecdsaMessageHash };
}

/**
 * Verify the supplier's signature over a RelayResponse, and NOTHING else.
 *
 * NOT SUFFICIENT ON ITS OWN — hence the name. It proves a supplier produced a
 * response with this session header and this payload HASH. It says nothing
 * about the payload sitting next to it, because the payload travels OUTSIDE the
 * signed bytes: skip the sha256(payload) comparison and anyone on the path can
 * swap the body for anything they like while this function still says VALID.
 *
 * Call openRelayResponse() unless you are deliberately checking one layer at a
 * time. It does this, then the binding, then the receipt, in that order.
 *
 * @param {Uint8Array} relayResponseBz the RelayResponse protobuf as received
 * @param {Uint8Array} supplierPubKey33 the supplier operator's compressed key
 * @returns {{
 *   valid: boolean, reason?: string,
 *   signableBytes: Uint8Array, signableHash: Uint8Array, ecdsaMessageHash: Uint8Array|null,
 *   payload: Uint8Array|null, payloadHash: Uint8Array|null,
 * }}
 */
export function verifyResponseSignatureOnly(relayResponseBz, supplierPubKey33) {
  const fields = parseRelayResponse(relayResponseBz);
  const signableBytes = filterSignableBytes(relayResponseBz);
  const signableHash = sha256(signableBytes);

  const base = {
    signableBytes,
    signableHash,
    ecdsaMessageHash: null,
    payload: fields.payload,
    payloadHash: fields.payloadHash,
  };

  if (!fields.supplierSignature) {
    return { ...base, valid: false, reason: 'response carries no supplier operator signature' };
  }

  const res = verifyCosmosSecp256k1(fields.supplierSignature, signableHash, supplierPubKey33);
  return { ...base, ecdsaMessageHash: res.ecdsaMessageHash, valid: res.valid, reason: res.reason };
}

/**
 * Check sha256(payload) against the payload_hash the signature committed to.
 *
 * This is the step that connects an authenticated hash to an unauthenticated
 * body, and it is the whole reason verifyResponseSignatureOnly() is not enough.
 *
 * A response with no payload_hash comes from a relayer older than poktroll
 * v0.1.25, which signed the payload directly — there is nothing to bind, so
 * this reports a match. filterSignableBytes() keeps the payload in the signed
 * bytes in exactly that case, so the signature is still covering the body.
 *
 * @param {Uint8Array|null} payload RelayResponse.Payload
 * @param {Uint8Array|null} payloadHash RelayResponse.PayloadHash
 * @returns {boolean}
 */
export function payloadBindingHolds(payload, payloadHash) {
  if (!payloadHash || payloadHash.length === 0) return true;
  if (payloadHash.length !== 32) return false;
  return bytesToHex(sha256(payload ?? new Uint8Array(0))) === bytesToHex(payloadHash);
}

// ---------------------------------------------------------------------------
// The inner POKTHTTPResponse.
// ---------------------------------------------------------------------------

/** Read a length-delimited string field's value as UTF-8. */
const utf8 = (bytes) => new TextDecoder('utf-8', { fatal: false }).decode(bytes);

/**
 * Parse RelayResponse.Payload, which is itself a marshalled POKTHTTPResponse.
 *
 * The answer a caller actually wants is TWO layers down: the relayer wraps the
 * backend's HTTP reply (status, headers, body) in a POKTHTTPResponse and puts
 * that marshalled message in RelayResponse.Payload. Reading Payload as JSON
 * "works" often enough to be dangerous — protobuf leaves the body bytes intact
 * and a lenient JSON parser may find them past the header noise — and then
 * fails on the first response whose headers happen to contain a brace.
 *
 * The header map is protobuf's map<string, Header>, which on the wire is a
 * repeated message: each entry is {key=1: string, value=2: Header}, and Header
 * is {key=1: string, values=2: repeated string}. The key is therefore present
 * twice; we trust the map entry's.
 *
 * Two consequences of it being a MAP, both of which bite ports that assume a
 * list. First, entry order on the wire is not meaningful: a Go encoder without
 * `Deterministic: true` emits them in random map order, so anything that
 * compares serialised headers positionally passes locally and flakes in
 * production. This function keys by name and never looks at position. Second,
 * one key can carry several values (`X-Backend-Node: a, b` is ONE entry with a
 * repeated `values` field, not two entries), which is why every value here is
 * an array even when it holds one string.
 *
 * @param {Uint8Array} payload RelayResponse.Payload
 * @returns {{status: number, headers: Record<string, string[]>, body: Uint8Array, bodyText: string}}
 */
export function parsePoktHttpResponse(payload) {
  let status = 0;
  const headers = {};
  let body = new Uint8Array(0);

  for (const rec of splitProtoFields(payload)) {
    switch (rec.field) {
      case F_HTTP_STATUS:
        if (rec.wireType !== 0) throw new Error(`status_code has wire type ${rec.wireType}, expected varint`);
        status = readVarint(payload, rec.valueStart).value;
        break;

      case F_HTTP_HEADER: {
        const entry = valueOf(payload, rec);
        let key = null;
        let values = [];
        for (const e of splitProtoFields(entry)) {
          if (e.field === 1) key = utf8(valueOf(entry, e));
          if (e.field === 2) {
            const header = valueOf(entry, e);
            for (const h of splitProtoFields(header)) {
              if (h.field === 2) values.push(utf8(valueOf(header, h)));
            }
          }
        }
        if (key !== null) headers[key] = values;
        break;
      }

      case F_HTTP_BODY:
        body = valueOf(payload, rec);
        break;

      default:
        break;
    }
  }
  return { status, headers, body, bodyText: utf8(body) };
}

// ---------------------------------------------------------------------------
// The receipt.
// ---------------------------------------------------------------------------

/**
 * Build the 32 bytes a receipt signs: sha256(domain tag || request signature ||
 * response payload hash).
 *
 * There are no length prefixes and none are needed: the tag is 22 fixed bytes
 * and the payload hash is 32, so the request signature's extent is pinned from
 * both ends. That is a property of THIS layout — add a second variable-length
 * field and the concatenation stops being injective, which is the classic way
 * domain-separated digests get broken.
 *
 * The request signature is the 199-byte bLSAG from RelayRequest.Meta.Signature:
 * the very bytes sign.mjs produced. That is what makes the receipt a reply
 * rather than an assertion — nobody can produce it before seeing your request.
 *
 * @param {Uint8Array} requestSignature RelayRequest.Meta.Signature, as sent
 * @param {Uint8Array} responsePayloadHash RelayResponse.PayloadHash, 32 bytes
 * @returns {{digest: Uint8Array, preimage: Uint8Array}}
 */
export function receiptDigest(requestSignature, responsePayloadHash) {
  if (!(requestSignature instanceof Uint8Array) || requestSignature.length === 0) {
    throw new Error('requestSignature must be the non-empty bLSAG signature bytes you sent');
  }
  if (!(responsePayloadHash instanceof Uint8Array) || responsePayloadHash.length !== 32) {
    throw new Error(`responsePayloadHash must be 32 bytes, got ${responsePayloadHash?.length ?? 0}`);
  }
  const preimage = concatBytes(RECEIPT_DOMAIN_TAG, requestSignature, responsePayloadHash);
  return { digest: sha256(preimage), preimage };
}

/**
 * Parse a `Pocket-Relay-Receipt` header value into signature bytes.
 *
 * Shape is `v1.` followed by 128 hex characters. The version prefix is not
 * decoration: a future v2 will change what the digest covers, so a parser that
 * skips the prefix and hexes the rest would verify a v2 receipt against v1
 * rules and report a confident, wrong answer. Reject what you do not know.
 *
 * Hex case is not specified by the header; the relayer emits lowercase and we
 * accept either, because normalising input is cheap and rejecting a valid
 * receipt over letter case is not a security property.
 *
 * @param {string} headerValue
 * @returns {{version: string, signature: Uint8Array}}
 */
export function parseReceiptHeader(headerValue) {
  if (typeof headerValue !== 'string') throw new Error(`${RECEIPT_HEADER} value must be a string`);
  const trimmed = headerValue.trim();

  const dot = trimmed.indexOf('.');
  if (dot < 0) throw new Error(`${RECEIPT_HEADER} value has no version prefix; expected "v1.<128 hex chars>"`);

  const version = trimmed.slice(0, dot);
  if (version !== 'v1') {
    throw new Error(`unsupported ${RECEIPT_HEADER} version ${JSON.stringify(version)}; this verifier only knows v1`);
  }

  const hex = trimmed.slice(dot + 1);
  if (hex.length !== 128) {
    throw new Error(`${RECEIPT_HEADER} signature must be 128 hex chars (64 bytes), got ${hex.length} chars`);
  }
  if (!/^[0-9a-fA-F]+$/.test(hex)) {
    // Distinguished from the length error on purpose: a 128-char value that is
    // not hex usually means base64, and reporting "wrong length" would send you
    // looking in the wrong place.
    throw new Error(`${RECEIPT_HEADER} signature is 128 chars but not hex (base64 by mistake?)`);
  }
  return { version, signature: hexToBytes(hex.toLowerCase()) };
}

/**
 * Verify a relay receipt.
 *
 * Pass the header value you received, the signature you SENT, and the payload
 * hash from the response you received. All three come from different places on
 * purpose: the receipt proves the supplier saw your request and produced this
 * response, and it can only prove that if you supply your own half from your
 * own records rather than from the response.
 *
 * @param {object} args
 * @param {string} [args.headerValue] the raw `Pocket-Relay-Receipt` value
 * @param {Uint8Array} [args.signature] or the 64 raw signature bytes
 * @param {Uint8Array} args.requestSignature the bLSAG signature you sent
 * @param {Uint8Array} args.responsePayloadHash RelayResponse.PayloadHash
 * @param {Uint8Array} args.supplierPubKey33 the supplier operator's compressed key
 * @returns {{valid: boolean, reason?: string, digest: Uint8Array, preimage: Uint8Array, ecdsaMessageHash: Uint8Array|null}}
 */
export function verifyReceipt({ headerValue, signature, requestSignature, responsePayloadHash, supplierPubKey33 }) {
  const { digest, preimage } = receiptDigest(requestSignature, responsePayloadHash);

  let sig = signature;
  if (sig === undefined) {
    if (headerValue === undefined) throw new Error('verifyReceipt needs either headerValue or signature');
    try {
      sig = parseReceiptHeader(headerValue).signature;
    } catch (err) {
      return { valid: false, reason: err.message, digest, preimage, ecdsaMessageHash: null };
    }
  }

  const res = verifyCosmosSecp256k1(sig, digest, supplierPubKey33);
  // ecdsaMessageHash is surfaced, not just used, because it is the value a raw
  // ECDSA verifier must be fed. Handing it `digest` instead fails while Go
  // insists the receipt is fine, and with nothing to compare against you cannot
  // tell which of the two hashes you got wrong. The oracle publishes this
  // number for exactly that reason.
  return { valid: res.valid, reason: res.reason, digest, preimage, ecdsaMessageHash: res.ecdsaMessageHash };
}

// ---------------------------------------------------------------------------
// The whole return trip.
// ---------------------------------------------------------------------------

/**
 * Check everything a caller can check, and hand back the backend's answer.
 *
 * THIS is the function to copy. The order is not cosmetic:
 *
 *   1. the supplier operator signature, over the filtered bytes;
 *   2. sha256(payload) == payload_hash — WITHOUT THIS THE SIGNATURE CHECK MEANS
 *      NOTHING, because the payload is not in the signed bytes;
 *   3. the receipt, if one was asked for and returned, binding this response to
 *      the request that produced it;
 *   4. only then, parse the payload.
 *
 * Named arguments rather than four positionals on purpose: `requestSignature`
 * and `receiptHeader` come from opposite ends of the exchange, and swapping
 * them silently is the kind of mistake this file exists to prevent.
 *
 * @param {object} args
 * @param {Uint8Array} args.relayResponseBz the RelayResponse protobuf as received
 * @param {Uint8Array} args.supplierPubKey33 the supplier operator's compressed key,
 *   read FROM CHAIN. Taking it out of the response verifies nothing at all.
 * @param {Uint8Array} [args.requestSignature] the bLSAG signature you sent. Only
 *   needed for the receipt.
 * @param {string} [args.receiptHeader] the `Pocket-Relay-Receipt` value, or
 *   undefined. A relayer returns one only when the request carried
 *   `Pocket-Sign-Receipt: true`, so absent is normal. If you DID ask for one,
 *   treat its absence as a failure at the call site: this function cannot tell
 *   "not requested" from "dropped in transit".
 * @returns {{
 *   valid: boolean, reason?: string,
 *   http: {status: number, headers: Record<string, string[]>, body: Uint8Array, bodyText: string}|null,
 *   signableBytes: Uint8Array, signableHash: Uint8Array, ecdsaMessageHash: Uint8Array|null,
 *   payload: Uint8Array|null, payloadHash: Uint8Array|null, payloadHashMatches: boolean,
 *   receipt: object|null,
 * }} Never throws: every input comes off the network, so malformed is an answer.
 */
export function openRelayResponse({ relayResponseBz, supplierPubKey33, requestSignature, receiptHeader }) {
  let sig;
  try {
    sig = verifyResponseSignatureOnly(relayResponseBz, supplierPubKey33);
  } catch (err) {
    return {
      valid: false,
      reason: `not a well-formed RelayResponse: ${err.message}`,
      http: null,
      signableBytes: new Uint8Array(0),
      signableHash: new Uint8Array(0),
      ecdsaMessageHash: null,
      payload: null,
      payloadHash: null,
      payloadHashMatches: false,
      receipt: null,
    };
  }

  const out = { ...sig, http: null, payloadHashMatches: false, receipt: null };

  // (1)
  if (!sig.valid) return { ...out, valid: false, reason: sig.reason };

  // (2)
  out.payloadHashMatches = payloadBindingHolds(sig.payload, sig.payloadHash);
  if (!out.payloadHashMatches) {
    return {
      ...out,
      valid: false,
      reason: 'sha256(payload) != payload_hash — the body does not belong to this signed response',
    };
  }

  // (3). receiptDigest() throws on YOUR OWN bad inputs — an empty request
  // signature, or a response with no payload_hash to bind to. Those are call
  // site bugs rather than hostile input, but a receipt that cannot even be
  // built is still a receipt that does not check out, so report it as one.
  if (receiptHeader !== undefined && receiptHeader !== null) {
    try {
      out.receipt = verifyReceipt({
        headerValue: receiptHeader,
        requestSignature,
        responsePayloadHash: sig.payloadHash,
        supplierPubKey33,
      });
    } catch (err) {
      return { ...out, valid: false, reason: `receipt cannot be checked: ${err.message}` };
    }
    if (!out.receipt.valid) {
      return { ...out, valid: false, reason: `receipt does not bind this response to that request: ${out.receipt.reason}` };
    }
  }

  // (4)
  try {
    out.http = parsePoktHttpResponse(sig.payload ?? new Uint8Array(0));
  } catch (err) {
    // Not every transport wraps a POKTHTTPResponse — a WebSocket subscription
    // frame does not — so this is a legitimate outcome for a response whose
    // signature and receipt both checked out.
    return { ...out, valid: false, reason: `payload is not a POKTHTTPResponse: ${err.message}` };
  }

  return { ...out, valid: true };
}

// ---------------------------------------------------------------------------
// Report.
// ---------------------------------------------------------------------------

/**
 * Check everything in one oracle `response-vectors` document and describe it.
 *
 * Every value printed here is also CHECKED, against the oracle's own numbers,
 * and every check that can be inverted is inverted: an edited payload, an
 * edited session header, a receipt from another response, a receipt from
 * another request, and a negated S. A walkthrough that only ever demonstrates
 * success teaches a reader nothing about what any of it proves — and a verifier
 * that returned true unconditionally would produce an identical report.
 *
 * Exported so the harness and any port can reuse the same walkthrough.
 *
 * @param {object} vectors parsed `oracle response-vectors` JSON
 * @returns {{ok: boolean, lines: string[]}} the verdict, and the human-readable
 *   report. The verdict is STRUCTURED: never re-derive it by searching these
 *   lines for a word, or rewording a label silently disables the check.
 */
export function report(vectors) {
  const lines = [];
  const say = (label, value) => lines.push(`  ${label.padEnd(26)} ${value}`);

  let ok = true;
  const check = (label, passed, detail = '') => {
    ok = ok && passed;
    lines.push(`  [${passed ? 'ok' : 'FAIL'}] ${label}${detail ? `  ${detail}` : ''}`);
    return passed;
  };

  const pub = hexToBytes(vectors.supplier.pub_hex);
  const relayResponse = hexToBytes(vectors.relay_response_hex);
  const controls = vectors.negative_controls;
  const reqSig = hexToBytes(vectors.receipt.request_signature_hex);

  lines.push('RelayResponse');
  const fields = parseRelayResponse(relayResponse);
  say('bytes received', `${relayResponse.length}`);
  say('payload', `${fields.payload?.length ?? 0} bytes`);
  say('payload_hash', bytesToHex(fields.payloadHash ?? new Uint8Array(0)));
  say('supplier signature', `${fields.supplierSignature?.length ?? 0} bytes`);
  // A response without one fails the relayer's own ValidateBasic, so a caller
  // will not meet it -- but the session-header negative control below reads it,
  // and a control that quietly skips itself is worse than no control.
  check('carries a session header', fields.sessionHeader !== null);

  lines.push('', 'Supplier response signature');
  const sig = verifyResponseSignatureOnly(relayResponse, pub);
  say('signable bytes (filtered)', `${sig.signableBytes.length} bytes  ${bytesToHex(sig.signableBytes).slice(0, 24)}...`);
  say('sha256(signable)', bytesToHex(sig.signableHash));
  say('sha256 of THAT (ECDSA)', bytesToHex(sig.ecdsaMessageHash ?? new Uint8Array(0)));
  check(
    'filtered signable bytes match the oracle',
    bytesToHex(sig.signableBytes) === vectors.signable_bytes_hex,
    `${sig.signableBytes.length} bytes, ${relayResponse.length - sig.signableBytes.length} filtered out`,
  );
  // The double hash, pinned against the number the oracle publishes rather than
  // inferred from the signature happening to verify.
  check(
    'the hash raw ECDSA covers is sha256 of that again',
    bytesToHex(sig.ecdsaMessageHash ?? new Uint8Array(0)) === vectors.response_ecdsa_message_hash_hex,
  );
  check('payload hashes to payload_hash', payloadBindingHolds(fields.payload, fields.payloadHash));
  check('supplier operator signature verifies', sig.valid, sig.reason ?? '');

  // Being unable to fail is the failure mode a signature check hides best. The
  // two ways a response can be altered are caught by two DIFFERENT mechanisms,
  // so break each in turn and watch the right one fire.
  const flipABit = (needle) => {
    const at = indexOfBytes(relayResponse, needle);
    if (at < 0) throw new Error('that needle is not in this response');
    const edited = Uint8Array.from(relayResponse);
    edited[at] ^= 0x01;
    return edited;
  };
  const open = (bz, extra = {}) => openRelayResponse({ relayResponseBz: bz, supplierPubKey33: pub, ...extra });

  check(
    '...and rejects an edited payload (the hash binding catches it)',
    !open(flipABit(fields.payload)).valid,
  );
  const serviceId = sessionHeaderServiceId(fields.sessionHeader);
  check(
    '...and rejects an edited session header (the signature catches it)',
    Boolean(serviceId) && !open(flipABit(serviceId)).valid,
  );

  lines.push('', 'Backend answer (inside RelayResponse.Payload)');
  if (fields.payload) {
    const inner = parsePoktHttpResponse(fields.payload);
    const want = vectors.inner_pokt_http_response;
    say('HTTP status', String(inner.status));
    for (const [k, v] of Object.entries(inner.headers)) say(`header ${k}`, v.join(', '));
    say('body', inner.bodyText);
    check('status matches the oracle', inner.status === want.status_code);
    check('body matches the oracle', bytesToHex(inner.body) === want.body_bz_hex);
    // Keys sorted on both sides: a protobuf map has no wire order, so comparing
    // the serialisations positionally would be asserting on the oracle's map
    // iteration rather than on this parser.
    const canonical = (h) => JSON.stringify(Object.fromEntries(Object.entries(h).sort(([a], [b]) => (a < b ? -1 : 1))));
    check('headers match the oracle', canonical(inner.headers) === canonical(want.headers));
  } else {
    say('payload', 'absent (an error response, or a claim/proof copy with the payload stripped)');
  }

  lines.push('', `Receipt (${RECEIPT_HEADER})`);
  // A receipt is opt-in: absent unless the request carried
  // `Pocket-Sign-Receipt: true`, and it cannot be built without payload_hash.
  const canCheckReceipt = Boolean(vectors.receipt?.header_value && fields.payloadHash);
  const receipt = canCheckReceipt
    ? verifyReceipt({
        headerValue: vectors.receipt.header_value,
        requestSignature: reqSig,
        responsePayloadHash: fields.payloadHash,
        supplierPubKey33: pub,
      })
    : null;

  if (receipt) {
    say('domain tag', `${RECEIPT_DOMAIN_TAG.length} bytes  ${bytesToHex(RECEIPT_DOMAIN_TAG)}`);
    say('preimage', `${receipt.preimage.length} bytes`);
    say('digest', bytesToHex(receipt.digest));
    say('sha256 of THAT (ECDSA)', bytesToHex(receipt.ecdsaMessageHash ?? new Uint8Array(0)));
    check('digest matches the oracle', bytesToHex(receipt.digest) === vectors.receipt.digest_hex);
    check(
      'the hash raw ECDSA covers matches the oracle',
      bytesToHex(receipt.ecdsaMessageHash ?? new Uint8Array(0)) === vectors.receipt.ecdsa_message_hash_hex,
    );
    check('receipt verifies against the supplier key', receipt.valid, receipt.reason ?? '');

    // The receipt is only worth something if it fails when either half of the
    // pair it commits to is swapped. Demonstrated, not asserted in prose.
    const receiptFor = (requestSignature, responsePayloadHash) =>
      verifyReceipt({
        headerValue: vectors.receipt.header_value,
        requestSignature,
        responsePayloadHash,
        supplierPubKey33: pub,
      }).valid;

    check('...and fails for a different response', !receiptFor(reqSig, hexToBytes(controls.other_payload_hash_hex)));
    check('...and fails for a different request', !receiptFor(hexToBytes(controls.other_request_signature_hex), fields.payloadHash));

    // (r, N-s) is as valid as (r, s) to the curve arithmetic; cosmos rejects it
    // on the low-S rule alone, and so does verifyCosmosSecp256k1.
    const parsed = secp256k1.Signature.fromBytes(parseReceiptHeader(vectors.receipt.header_value).signature);
    const highS = new secp256k1.Signature(parsed.r, N - parsed.s).toBytes();
    check(
      '...and fails when S is negated (the low-S rule)',
      !verifyReceipt({
        signature: highS,
        requestSignature: reqSig,
        responsePayloadHash: fields.payloadHash,
        supplierPubKey33: pub,
      }).valid,
    );
  } else {
    say('receipt', `absent — send "${RECEIPT_REQUEST_HEADER}: true" with the request to get one`);
  }

  // Everything above took the response apart. This is the one call a caller
  // actually makes: all four steps, in order, and the backend's answer out the
  // other side.
  lines.push('', 'The whole return trip, in one call');
  const opened = open(relayResponse, { requestSignature: reqSig, receiptHeader: vectors.receipt?.header_value });
  check('openRelayResponse accepts response + receipt', opened.valid, opened.reason ?? '');
  check(
    '...and returns the backend\'s own body',
    Boolean(opened.http) && bytesToHex(opened.http.body) === vectors.inner_pokt_http_response.body_bz_hex,
  );
  check(
    '...and refuses a receipt from a different relay',
    !open(relayResponse, {
      requestSignature: hexToBytes(controls.other_request_signature_hex),
      receiptHeader: vectors.receipt?.header_value,
    }).valid,
  );

  lines.push('', ok ? 'PASS' : 'FAIL');
  return { ok, lines };
}

/** Offset of `needle` in `haystack`, or -1. Uint8Array has no indexOf for this. */
function indexOfBytes(haystack, needle) {
  if (!needle || needle.length === 0) return -1;
  outer: for (let i = 0; i + needle.length <= haystack.length; i++) {
    for (let j = 0; j < needle.length; j++) if (haystack[i + j] !== needle[j]) continue outer;
    return i;
  }
  return -1;
}

/**
 * The `service_id` out of a marshalled SessionHeader, or null.
 *
 * Used only by the report, to flip a bit somewhere the message stays a
 * well-formed protobuf: corrupting a tag byte instead would be caught by the
 * PARSER, which proves nothing about the signature.
 */
function sessionHeaderServiceId(sessionHeader) {
  if (!sessionHeader) return null;
  for (const rec of splitProtoFields(sessionHeader)) {
    if (rec.field === F_HDR_SERVICE_ID) return valueOf(sessionHeader, rec);
  }
  return null;
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

/**
 * True when this file was run directly rather than imported.
 *
 * realpath on both sides so a symlinked or relatively-invoked script still
 * matches; comparing import.meta.url to a hand-built file:// URL does not, and
 * gets it wrong for any path containing a space.
 */
const isMain = Boolean(process.argv[1]) && realpathSync(process.argv[1]) === realpathSync(import.meta.filename);

if (isMain) {
  const { spawnSync } = await import('node:child_process');
  const { dirname, resolve } = await import('node:path');

  const here = dirname(import.meta.filename);

  // Read piped vectors if there are any. isTTY alone is not enough: under CI,
  // make, or a test runner, stdin is usually /dev/null — not a TTY and not a
  // document either — and blindly JSON.parse('') there is a confusing crash.
  let raw = '';
  if (!process.stdin.isTTY) {
    try {
      raw = readFileSync(0, 'utf8');
    } catch {
      raw = ''; // EAGAIN on an idle pipe: treat as "nothing piped in"
    }
  }

  if (raw.trim() === '') {
    // Produce the vectors ourselves so `node verify.mjs` just works. The oracle
    // is the authority either way.
    const oraclePath = process.env.ORACLE ?? resolve(here, 'oracle-bin');
    const res = spawnSync(oraclePath, ['response-vectors'], { encoding: 'utf8' });
    if (res.error?.code === 'ENOENT') {
      console.error(
        `\nCannot find the oracle at ${oraclePath}\n\n` +
          'Build it, or pipe its output in:\n\n' +
          `  go build -o ${oraclePath} ${resolve(here, '../oracle')}\n` +
          `  ${oraclePath} response-vectors | node verify.mjs\n`,
      );
      process.exit(2);
    }
    if (res.error) throw res.error;
    if (res.status !== 0) {
      console.error(`oracle response-vectors exited ${res.status}: ${res.stderr}`);
      process.exit(2);
    }
    raw = res.stdout;
  }

  const vectors = JSON.parse(raw);
  const { ok, lines } = report(vectors);
  console.log(`\n${lines.join('\n')}\n`);

  // Exit non-zero on a bad verdict: this file is run in pipelines, and a
  // verifier that prints a failure and exits 0 is a verifier nobody notices.
  //
  // The verdict comes from the STRUCTURED result, never from grepping the text
  // above for a word. It used to search the rendered lines for "INVALID", which
  // meant that rewording one label silently pinned this exit code to zero
  // forever — a green pipeline over a verifier that had stopped verifying.
  process.exit(ok ? 0 : 1);
}
