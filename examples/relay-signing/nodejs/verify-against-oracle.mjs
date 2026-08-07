#!/usr/bin/env node
/**
 * Test harness: sign with sign.mjs, verify with verify.mjs, judge both with the
 * real Go implementations.
 *
 * Round-tripping your own sign -> your own verify proves nothing. An
 * implementation can be perfectly self-consistent and still be rejected by the
 * relayer for its entire life. So every signature here is handed to the oracle,
 * which runs the same github.com/pokt-network/ring-go the relayer runs.
 *
 * Steps 1-4 cover the outbound half (sign.mjs). Step 5 covers the return trip
 * (verify.mjs): the supplier's response signature and the relay receipt, both
 * checked against bytes the oracle produced with the real poktroll and
 * shannon-sdk types, and both with negative controls that must FAIL.
 *
 * WHY 1500 SIGNATURES, AND WHY YOU MUST NOT LOWER IT
 * --------------------------------------------------
 * The left-align quirk in hashToScalar fires on ~1 hash in 256. A 2-member ring
 * derives 2 challenges per signature, so an implementation that gets the quirk
 * wrong still produces a VALID signature ~99.2% of the time. At 10 signatures a
 * broken port passes with probability 0.92; at 50, 0.68. It looks finished, it
 * ships, and it silently loses ~1 relay in 128 forever.
 *
 * 1500 signatures puts P(a broken port passing) at ~1e-5. That is the whole
 * reason this file exists rather than a quick 10-message smoke test.
 *
 * Step 4 is the negative control: it deliberately signs with the canonical
 * (right-aligning) derivation and requires the oracle to REJECT some. Without
 * it, a harness that had quietly stopped exercising the quirk — wrong oracle,
 * vectors regenerated, signatures not actually reaching the verifier — would
 * report a green 1500/1500 and mean nothing.
 *
 * Usage:
 *   go build -o ./oracle-bin ../oracle && ORACLE=./oracle-bin node verify-against-oracle.mjs
 *
 * Takes ~1 minute: 3000 signatures, each verified in its own oracle process.
 * Step 5 is deterministic and adds under a second.
 */

import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { sha3_256, sha3_512 } from '@noble/hashes/sha3.js';
import { sha256 } from '@noble/hashes/sha2.js';
import { secp256k1 } from '@noble/curves/secp256k1.js';
import { bytesToHex, bytesToNumberBE, hexToBytes, randomBytes } from '@noble/curves/utils.js';

import { buildRelaySignature, encodeScalar, hashToCurve, hashToScalar } from './sign.mjs';
import {
  RECEIPT_DOMAIN_TAG,
  filterSignableBytes,
  openRelayResponse,
  parsePoktHttpResponse,
  parseRelayResponse,
  parseReceiptHeader,
  receiptDigest,
  verifyReceipt,
  verifyResponseSignatureOnly,
} from './verify.mjs';

const N = secp256k1.Point.Fn.ORDER;
const TRIALS = 1500;

const here = dirname(fileURLToPath(import.meta.url));
const ORACLE = process.env.ORACLE ?? resolve(here, 'oracle-bin');

let failures = 0;
const fail = (msg) => {
  failures++;
  console.error(`  FAIL  ${msg}`);
};
const pass = (msg) => console.log(`  ok    ${msg}`);

/** Run the oracle. Returns {status, stdout}; a non-zero status is data, not an error. */
function oracle(args, input) {
  const res = spawnSync(ORACLE, args, { input, encoding: 'utf8' });
  if (res.error?.code === 'ENOENT') {
    console.error(
      `\nCannot find the oracle at ${ORACLE}\n\n` +
        'The oracle is the authority this harness tests against. Build it:\n\n' +
        `  go build -o ${ORACLE} ${resolve(here, '../oracle')}\n\n` +
        'or point ORACLE=/path/to/oracle at an existing build.\n',
    );
    process.exit(2);
  }
  if (res.error) throw res.error;
  return res;
}

// ---------------------------------------------------------------------------
// Step 1: SHA-3 is FIPS-202, not Keccak.
// ---------------------------------------------------------------------------
// Both are exported by @noble/hashes under confusingly adjacent names. Picking
// the wrong one produces a well-formed signature that is always rejected, with
// no diagnostic anywhere. These are the NIST FIPS-202 known-answer values for
// the empty input, so this pins the choice independently of the oracle.
console.log('\n[1/5] SHA-3 is FIPS-202, not Keccak');
{
  const KAT = {
    sha3_512:
      'a69f73cca23a9ac5c8b567dc185a756e97c982164fe25859e0d1dcc1475c80a6' +
      '15b2123af1f5f94c11e3e9402c3ac558f500199d95b6d3e301758586281dcd26',
    sha3_256: 'a7ffc6f8bf1ed76651c14756a061d662f580ff4de43b49fa82d80a4b80f8434a',
  };
  const got512 = bytesToHex(sha3_512(new Uint8Array(0)));
  const got256 = bytesToHex(sha3_256(new Uint8Array(0)));

  got512 === KAT.sha3_512
    ? pass('sha3_512("") matches the NIST FIPS-202 known-answer test')
    : fail(`sha3_512("") = ${got512}\n        want NIST FIPS-202 ${KAT.sha3_512}\n        (if this is 0eab42de... you imported keccak_512)`);

  got256 === KAT.sha3_256
    ? pass('sha3_256("") matches the NIST FIPS-202 known-answer test')
    : fail(`sha3_256("") = ${got256}\n        want NIST FIPS-202 ${KAT.sha3_256}\n        (if this is c5d24601... you imported keccak_256)`);
}

// ---------------------------------------------------------------------------
// Step 2: the deterministic vectors.
// ---------------------------------------------------------------------------
// A signature is randomized and cannot be diffed, so when step 3 fails these
// are what tell you WHICH primitive drifted.
console.log('\n[2/5] Deterministic vectors from `oracle vectors`');
const vectors = JSON.parse(oracle(['vectors']).stdout);
{
  // Guard the guard: the SHORT-reduction probe is the only vector that can
  // catch a canonical hashToScalar. If it ever disappears from the oracle's
  // output, every port silently loses its one deterministic check on the quirk.
  const short = vectors.hash_to_scalar.filter((v) => v.note?.includes('SHORT'));
  short.length > 0
    ? pass(`vector file still pins the quirk (${short.length} SHORT-reduction probe)`)
    : fail('no SHORT-reduction vector in `oracle vectors` — the quirk is no longer pinned deterministically');

  for (const v of vectors.hash_to_scalar) {
    const got = bytesToHex(encodeScalar(hashToScalar(hexToBytes(v.input_hex))));
    const isShort = v.note?.includes('SHORT');
    got === v.output_hex
      ? pass(`hash_to_scalar ${v.input_hex}${isShort ? '  <- the SHORT reduction; a canonical port fails HERE' : ''}`)
      : fail(`hash_to_scalar(${v.input_hex})\n        got  ${got}\n        want ${v.output_hex}`);
  }

  for (const v of vectors.hash_to_curve) {
    const got = bytesToHex(hashToCurve(hexToBytes(v.input_hex)).toBytes(true));
    got === v.output_hex
      ? pass(`hash_to_curve  ${v.input_hex.slice(0, 18)}...`)
      : fail(`hash_to_curve(${v.input_hex})\n        got  ${got}\n        want ${v.output_hex}`);
  }

  for (const v of vectors.scalar_encoding) {
    const got = bytesToHex(encodeScalar(bytesToNumberBE(hexToBytes(v.input_hex))));
    got === v.output_hex
      ? pass('scalar_encoding is right-aligned on the wire (the opposite of hash_to_scalar)')
      : fail(`encodeScalar(0x${v.input_hex})\n        got  ${got}\n        want ${v.output_hex}`);
  }
}

// The relay ring: [app, gateway], in that order, gateway signs.
const appPub = hexToBytes(vectors.keys.app_pub_hex);
const gwPub = hexToBytes(vectors.keys.gateway_pub_hex);
const gwPriv = hexToBytes(vectors.keys.gateway_priv_hex);
const ring = [appPub, gwPub];
const OUR_IDX = 1;

/** Sign, hand the bytes to the Go verifier, report what it says. */
function oracleAccepts(m, sig) {
  const res = oracle(['verify'], JSON.stringify({ msg_hex: bytesToHex(m), sig_hex: bytesToHex(sig) }));
  return { accepted: res.status === 0, reason: JSON.parse(res.stdout || '{}').reason };
}

// ---------------------------------------------------------------------------
// Step 3: sign 1500 distinct messages; the Go verifier must accept every one.
// ---------------------------------------------------------------------------
console.log(`\n[3/5] ${TRIALS} distinct messages -> ring-go must accept 100%`);
{
  const messages = Array.from({ length: TRIALS }, () => randomBytes(32));
  const distinct = new Set(messages.map(bytesToHex)).size;
  distinct === TRIALS
    ? pass(`${TRIALS} messages, all distinct`)
    : fail(`only ${distinct}/${TRIALS} messages were distinct`);

  const rejected = [];
  let badLength = 0;
  const t0 = Date.now();

  for (const [i, m] of messages.entries()) {
    const sig = buildRelaySignature(m, ring, gwPriv, OUR_IDX);
    if (sig.length !== 199) badLength++;

    const { accepted, reason } = oracleAccepts(m, sig);
    if (!accepted) rejected.push({ i, msg: bytesToHex(m), sig: bytesToHex(sig), reason });

    if ((i + 1) % 250 === 0) {
      process.stdout.write(`        ${i + 1}/${TRIALS} signed and verified, ${rejected.length} rejected\n`);
    }
  }
  const secs = ((Date.now() - t0) / 1000).toFixed(1);

  badLength === 0
    ? pass('every signature is 199 bytes (69 + 65*2)')
    : fail(`${badLength} signatures had the wrong length`);

  if (rejected.length === 0) {
    pass(`ring-go accepted ${TRIALS}/${TRIALS} (100%) in ${secs}s`);
  } else {
    const rate = ((rejected.length / TRIALS) * 100).toFixed(2);
    fail(
      `ring-go rejected ${rejected.length}/${TRIALS} (${rate}%). ` +
        `A rate near 0.78% means hashToScalar is not reproducing the left-align quirk ` +
        `(see sign.mjs). Retrying will NOT help — the quirk is deterministic per input.\n` +
        `        first rejection: ${rejected[0].reason}\n` +
        `        msg ${rejected[0].msg}\n        sig ${rejected[0].sig}`,
    );
  }
}

// ---------------------------------------------------------------------------
// Step 4: negative control — prove this harness can actually detect the bug.
// ---------------------------------------------------------------------------
// Sign with the canonical (right-aligning) derivation, i.e. the implementation
// a careful person writes by instinct, and require the oracle to reject some.
// If this passes 1500/1500 then step 3's green is meaningless.
console.log(`\n[4/5] Negative control: ${TRIALS} signatures with a CANONICAL hashToScalar`);
{
  // The natural, wrong implementation: reduce mod N and stop. No left-align.
  const canonicalHashToScalar = (input) => bytesToNumberBE(sha3_512(input)) % N;

  let rejected = 0;
  for (let i = 0; i < TRIALS; i++) {
    const m = randomBytes(32);
    const sig = buildRelaySignature(m, ring, gwPriv, OUR_IDX, { hashToScalar: canonicalHashToScalar });
    if (!oracleAccepts(m, sig).accepted) rejected++;
  }

  const rate = (rejected / TRIALS) * 100;
  if (rejected === 0) {
    fail(
      `a canonical hashToScalar was accepted ${TRIALS}/${TRIALS} times. Expected ~0.78% ` +
        'rejections. Either this harness is not really reaching the Go verifier, or ' +
        'go-dleq dropped the left-align — in which case sign.mjs and its comments must ' +
        'change together, and every existing port breaks.',
    );
  } else if (rate > 5) {
    fail(
      `canonical rejection rate ${rate.toFixed(2)}% (${rejected}/${TRIALS}), expected ~0.78%. ` +
        'A rate this high means something broader than the left-align is wrong.',
    );
  } else {
    pass(
      `ring-go rejected ${rejected}/${TRIALS} = ${rate.toFixed(2)}% of canonical signatures ` +
        `(predicted ~0.78%) — the quirk is real, and step 3 is a meaningful check`,
    );
  }
}

// ---------------------------------------------------------------------------
// Step 5: the return trip — response signature and relay receipt.
// ---------------------------------------------------------------------------
// Everything above proves a relay gets ACCEPTED. This proves the caller can
// check what comes back. The vectors are built by the oracle with the real
// poktroll and shannon-sdk types, so a disagreement here is a disagreement with
// the relayer.
//
// The negative controls are half of this step and are not optional. A receipt
// that verifies is unremarkable; what makes it worth computing is that it FAILS
// when either input is swapped. Without the four rejections below, a verifier
// that ignored its inputs entirely and returned true would score a clean green.
console.log('\n[5/5] Return trip: response signature and relay receipt');
{
  const rv = JSON.parse(oracle(['response-vectors']).stdout);

  const supplierPub = hexToBytes(rv.supplier.pub_hex);
  const relayResponse = hexToBytes(rv.relay_response_hex);
  const reqSig = hexToBytes(rv.receipt.request_signature_hex);

  // --- the signable bytes are obtained by filtering, and must be byte-exact ---
  const filtered = filterSignableBytes(relayResponse);
  bytesToHex(filtered) === rv.signable_bytes_hex
    ? pass(`filtered signable bytes are byte-exact (${filtered.length} B from ${relayResponse.length} B received)`)
    : fail(
        `filterSignableBytes disagrees with the oracle.\n` +
          `        got  ${bytesToHex(filtered)}\n        want ${rv.signable_bytes_hex}\n` +
          '        A verifier that re-encodes instead of filtering fails exactly here.',
      );

  // --- the payload is committed to only through payload_hash ---
  const fields = parseRelayResponse(relayResponse);
  bytesToHex(fields.payloadHash ?? new Uint8Array(0)) === rv.payload_hash_hex
    ? pass('payload_hash read out of the received protobuf matches the oracle')
    : fail(`payload_hash mismatch: got ${bytesToHex(fields.payloadHash ?? new Uint8Array(0))}, want ${rv.payload_hash_hex}`);

  bytesToHex(sha256(fields.payload ?? new Uint8Array(0))) === rv.payload_hash_hex
    ? pass('sha256(payload) == payload_hash — the body is bound to the signed bytes')
    : fail('sha256(payload) != payload_hash; the body would be unauthenticated');

  // --- the response signature itself ---
  const respCheck = verifyResponseSignatureOnly(relayResponse, supplierPub);
  respCheck.valid
    ? pass('the supplier operator signature over the RelayResponse verifies')
    : fail(`response signature rejected: ${respCheck.reason}`);

  // Prove the double hash is doing something, rather than being an accident of
  // a library default. cosmos signs sha256(signable_hash); verifying against
  // the single hash must fail, and if it does not, the vectors are not what
  // this harness thinks they are.
  secp256k1.verify(hexToBytes(rv.response_signature_hex), respCheck.signableHash, supplierPub, {
    prehash: false,
    lowS: true,
  })
    ? fail('the response signature verified against a SINGLE sha256 — the cosmos double hash is not being exercised')
    : pass('the same signature is rejected against a single sha256 (the cosmos double hash is real)');

  // "It verified" only tells you the pair matched; it does not tell you WHICH
  // hash you fed the curve. The oracle publishes the exact value raw ECDSA
  // covers, so pin it. A port that had prehash and the cosmos double hash wrong
  // in compensating directions would pass every check above and fail here.
  bytesToHex(respCheck.ecdsaMessageHash) === rv.response_ecdsa_message_hash_hex
    ? pass('sha256(sha256(signable_bytes)) equals the oracle\'s response_ecdsa_message_hash_hex')
    : fail(
        `the value we hand raw ECDSA is not the oracle's.\n` +
          `        got  ${bytesToHex(respCheck.ecdsaMessageHash)}\n        want ${rv.response_ecdsa_message_hash_hex}`,
      );

  // --- the inner POKTHTTPResponse: the answer is two layers down ---
  const inner = parsePoktHttpResponse(fields.payload);
  inner.status === rv.inner_pokt_http_response.status_code
    ? pass(`inner POKTHTTPResponse status ${inner.status}`)
    : fail(`inner status ${inner.status}, want ${rv.inner_pokt_http_response.status_code}`);

  bytesToHex(inner.body) === rv.inner_pokt_http_response.body_bz_hex
    ? pass(`inner body_bz is the JSON-RPC answer: ${inner.bodyText}`)
    : fail(`inner body_bz ${bytesToHex(inner.body)}, want ${rv.inner_pokt_http_response.body_bz_hex}`);

  // Compare with the keys SORTED. A protobuf map has no wire order — Go emits
  // entries in random map order unless the encoder sets Deterministic — so
  // comparing JSON.stringify output directly would be asserting on the oracle's
  // map iteration, and would flake the day it stops sorting.
  const canonical = (h) =>
    JSON.stringify(Object.fromEntries(Object.entries(h).sort(([a], [b]) => (a < b ? -1 : 1))));
  const wantHeaders = canonical(rv.inner_pokt_http_response.headers);
  canonical(inner.headers) === wantHeaders
    ? pass(`inner header map decoded from ${Object.keys(inner.headers).length} protobuf map entries`)
    : fail(`inner headers ${canonical(inner.headers)}, want ${wantHeaders}`);

  // A repeated-value header is ONE map entry with a repeated `values` field,
  // not two entries. Until the fixture carried one, a parser that kept only the
  // last value per key passed everything. Assert it explicitly so the coverage
  // cannot quietly disappear if the fixture is simplified again.
  const multi = Object.entries(inner.headers).filter(([, v]) => v.length > 1);
  multi.length > 0
    ? pass(`repeated-value header exercised: ${multi.map(([k, v]) => `${k}=[${v.join(', ')}]`).join(' ')}`)
    : fail(
        'no header in the fixture carries more than one value, so multi-value parsing is untested — ' +
          'a parser that keeps only the last value would pass this whole step',
      );

  // --- the receipt ---
  bytesToHex(RECEIPT_DOMAIN_TAG) === rv.receipt.domain_tag_hex
    ? pass(`domain tag is the ${RECEIPT_DOMAIN_TAG.length} bytes of "POKT-RELAY-RECEIPT-v1\\0", trailing NUL included`)
    : fail(`domain tag ${bytesToHex(RECEIPT_DOMAIN_TAG)}, want ${rv.receipt.domain_tag_hex}`);

  const { digest, preimage } = receiptDigest(reqSig, fields.payloadHash);
  bytesToHex(preimage) === rv.receipt.preimage_hex
    ? pass(`receipt preimage is byte-exact (${preimage.length} B: 22 tag + ${reqSig.length} sig + 32 hash)`)
    : fail(`receipt preimage mismatch\n        got  ${bytesToHex(preimage)}\n        want ${rv.receipt.preimage_hex}`);

  bytesToHex(digest) === rv.receipt.digest_hex
    ? pass('receipt digest matches the oracle')
    : fail(`receipt digest ${bytesToHex(digest)}, want ${rv.receipt.digest_hex}`);

  const headerSig = parseReceiptHeader(rv.receipt.header_value).signature;
  bytesToHex(headerSig) === rv.receipt.signature_hex
    ? pass(`parsed the "${rv.receipt.header_value.slice(0, 3)}" header into 64 signature bytes`)
    : fail(`parsed header signature ${bytesToHex(headerSig)}, want ${rv.receipt.signature_hex}`);

  const good = verifyReceipt({
    headerValue: rv.receipt.header_value,
    requestSignature: reqSig,
    responsePayloadHash: fields.payloadHash,
    supplierPubKey33: supplierPub,
  });
  good.valid ? pass('the receipt verifies') : fail(`receipt rejected: ${good.reason}`);

  // Same pin as the response side: the digest is NOT what raw ECDSA covers.
  // Feed a bare ECDSA verifier `digest_hex` and it fails while Go swears the
  // receipt is valid — the single most expensive way to lose a day here.
  bytesToHex(good.ecdsaMessageHash) === rv.receipt.ecdsa_message_hash_hex
    ? pass('sha256(receipt digest) equals the oracle\'s receipt.ecdsa_message_hash_hex')
    : fail(
        `the value we hand raw ECDSA for the receipt is not the oracle's.\n` +
          `        got  ${bytesToHex(good.ecdsaMessageHash)}\n        want ${rv.receipt.ecdsa_message_hash_hex}`,
      );

  // And prove the two are genuinely different values, so the check above cannot
  // be satisfied by a verifier that confused one for the other.
  bytesToHex(good.digest) === rv.receipt.ecdsa_message_hash_hex
    ? fail('digest_hex and ecdsa_message_hash_hex are the same value; the double hash is not in these vectors')
    : pass('digest and ECDSA message hash are distinct values — feeding the wrong one WILL fail');

  // --- cross-check our digest against the real cosmos verifier ---
  // Our verifier and our digest could be wrong together and still agree. The Go
  // side takes the digest WE computed and judges it with cosmos secp256k1.
  const crossOk =
    oracle(['verify-receipt'], JSON.stringify({
      digest_hex: bytesToHex(digest),
      sig_hex: rv.receipt.signature_hex,
      pub_hex: rv.supplier.pub_hex,
    })).status === 0;
  crossOk
    ? pass('cosmos secp256k1 (via `oracle verify-receipt`) accepts the digest WE computed')
    : fail('the Go verifier rejected our digest — our receiptDigest disagrees with relayer/receipt.go');

  // --- negative control 1: a different response ---
  const wrongHash = verifyReceipt({
    headerValue: rv.receipt.header_value,
    requestSignature: reqSig,
    responsePayloadHash: hexToBytes(rv.negative_controls.other_payload_hash_hex),
    supplierPubKey33: supplierPub,
  });
  wrongHash.valid
    ? fail('the receipt verified against a DIFFERENT response payload hash — it proves nothing about which response you got')
    : pass('receipt REJECTED against a different response payload hash');

  // --- negative control 2: a different request ---
  const wrongReq = verifyReceipt({
    headerValue: rv.receipt.header_value,
    requestSignature: hexToBytes(rv.negative_controls.other_request_signature_hex),
    responsePayloadHash: fields.payloadHash,
    supplierPubKey33: supplierPub,
  });
  wrongReq.valid
    ? fail('the receipt verified against a DIFFERENT request signature — it is not bound to your request')
    : pass('receipt REJECTED against a different request signature');

  // --- negative control 3: low-S is enforced, here and in Go ---
  // Every (r, s) has an equally valid twin (r, N-s). Cosmos rejects the high
  // one; a verifier that does not lets an attacker mint a second byte-string
  // for the same authorised receipt.
  const parsed = secp256k1.Signature.fromBytes(hexToBytes(rv.receipt.signature_hex));
  const highS = new secp256k1.Signature(parsed.r, N - parsed.s).toBytes();

  const highLocal = verifyReceipt({
    signature: highS,
    requestSignature: reqSig,
    responsePayloadHash: fields.payloadHash,
    supplierPubKey33: supplierPub,
  });
  highLocal.valid
    ? fail('verify.mjs accepted a HIGH-S receipt signature; cosmos would reject it')
    : pass(`high-S receipt REJECTED locally (${highLocal.reason})`);

  const highGo =
    oracle(['verify-receipt'], JSON.stringify({
      digest_hex: bytesToHex(digest),
      sig_hex: bytesToHex(highS),
      pub_hex: rv.supplier.pub_hex,
    })).status === 0;
  highGo
    ? fail('cosmos accepted the high-S signature — the malleability assumption in verify.mjs is wrong')
    : pass('high-S receipt REJECTED by cosmos too, so verify.mjs matches the chain');

  // --- negative control 4: the domain tag's trailing NUL is load-bearing ---
  const noNul = new TextEncoder().encode('POKT-RELAY-RECEIPT-v1');
  const strippedDigest = sha256(
    Uint8Array.from([...noNul, ...reqSig, ...fields.payloadHash]),
  );
  bytesToHex(strippedDigest) === bytesToHex(digest)
    ? fail('dropping the trailing NUL produced the same digest; the tag length is not what this harness assumes')
    : pass('dropping the domain tag\'s trailing NUL changes the digest — the 22nd byte matters');

  // --- the entry point, end to end ---
  // Everything above checks one layer. openRelayResponse is the function a
  // reader copies, so the harness has to exercise it at full scope: all four
  // steps in order, and the backend's answer out the other side.
  const opened = openRelayResponse({
    relayResponseBz: relayResponse,
    supplierPubKey33: supplierPub,
    requestSignature: reqSig,
    receiptHeader: rv.receipt.header_value,
  });
  opened.valid && bytesToHex(opened.http?.body ?? new Uint8Array(0)) === rv.inner_pokt_http_response.body_bz_hex
    ? pass('openRelayResponse accepts response + receipt and returns the backend body')
    : fail(`openRelayResponse rejected the oracle's own response: ${opened.reason}`);

  // ...and refuses the same receipt once the request half is swapped, which is
  // the one thing the response signature alone could never tell you.
  openRelayResponse({
    relayResponseBz: relayResponse,
    supplierPubKey33: supplierPub,
    requestSignature: hexToBytes(rv.negative_controls.other_request_signature_hex),
    receiptHeader: rv.receipt.header_value,
  }).valid
    ? fail('openRelayResponse accepted a receipt from a DIFFERENT relay')
    : pass('openRelayResponse REJECTS a receipt from a different relay');
}

// ---------------------------------------------------------------------------
console.log(
  failures === 0
    ? `\nPASS — ${TRIALS} signatures accepted by ring-go, 100%. The quirk is replicated, ` +
        'and the return trip verifies with its negative controls rejected.\n'
    : `\nFAIL — ${failures} check(s) failed.\n`,
);
process.exit(failures === 0 ? 0 : 1);
