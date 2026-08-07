#!/usr/bin/env python3
"""Test sign.py and verify.py against the real Go libraries.

Round-tripping your own sign->verify proves nothing: an implementation can be
perfectly self-consistent and still be rejected by every relayer it ever talks
to. The only authority is the Go the relayer runs — github.com/pokt-network/ring-go
for the request signature, cosmos-sdk's secp256k1 for everything on the way
back — and the oracle in ../oracle wraps both. So every signature made here is
handed to it, and every verdict reached here is put to it as a question.

Usage:

    go build -o /tmp/oracle ../oracle/
    python3 verify_against_oracle.py --oracle /tmp/oracle

    python3 verify_against_oracle.py --count 5000     # more samples
    python3 verify_against_oracle.py --jobs 1         # serial, for debugging

The oracle is also found automatically at /tmp/oracle, at ./oracle-bin, on
$ORACLE, or on $PATH.

Why the sample count has a floor: hash_to_scalar's left-align quirk only shows
up when a reduction lands below 2^248, which is about 1 hash in 256. A
2-member ring hashes twice per signature, so a port that gets the quirk wrong
still passes ~99.2% of the time. Ten signatures tell you nothing. A thousand
and a half tell you something.

Steps 1 to 4 cover the outbound half, sign.py, in increasing order of how much
they prove:

  1. vectors        -- the deterministic primitives, so a failure below can be
                       bisected to the exact function that drifted
  2. go -> python   -- our verifier accepts a signature made by real Go, and
                       REJECTS a tampered one. Accepting alone is not a test:
                       a verifier hardcoded to say yes would pass it
  3. python -> go   -- real Go accepts `--count` of our signatures. 100%, no
                       retries. Also reports how often the quirk actually
                       fired, because a test that never hit it proved nothing
  4. negative ctrl  -- the same signatures, but with the quirk deliberately
                       removed, must get REJECTED. This is what shows the test
                       has teeth: without it, a vacuous harness that accepts
                       everything looks identical to a passing one

Steps 5 and 6 cover the return trip, verify.py, on the same principle. They are
deterministic and cost nothing, so --count does not apply to them:

  5. response       -- the bytes we filter out of a received RelayResponse must
                       equal the oracle's signable bytes, the signature must
                       verify, and edits to either the payload or the session
                       header must be caught
  6. receipt        -- our receipt digest must verify, must FAIL against a
                       different request and against a different response, and
                       every one of those verdicts is then put to the cosmos
                       library itself rather than trusted from our own maths
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import List, NamedTuple, Optional, Tuple

import sign
import verify

# The oracle's throwaway test keys (examples/relay-signing/oracle/main.go).
APP_PRIV = bytes.fromhex("2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a")
GW_PRIV = bytes.fromhex("1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11")

# ring = [app, gateway]; the gateway holds the key, so it signs at index 1.
RING = [sign.public_key_from_private(APP_PRIV), sign.public_key_from_private(GW_PRIV)]
OUR_IDX = 1

ORACLE_BIN = "oracle"  # set by main(), and re-set in workers by _init_worker()


class Result(NamedTuple):
    """One signature's trip through the Go verifier."""

    index: int
    accepted: bool
    reason: str
    quirk_hits: int  # short reductions seen while building this signature
    msg_hex: str
    sig_hex: str


# --------------------------------------------------------------------------
# Instrumentation
#
# `1500/1500 accepted` is only meaningful if the runs actually exercised the
# quirk. These wrappers count how often the trap fired, and let the negative
# control disable the quirk to prove the harness would notice.
# --------------------------------------------------------------------------

_QUIRK_HITS = 0

_real_hash_to_scalar = sign.hash_to_scalar


def _reduction_is_short(data: bytes) -> bool:
    """True when SHA3-512(data) mod N needs fewer than 32 bytes: the quirk case."""
    n = int.from_bytes(hashlib.sha3_512(data).digest(), "big") % sign.N
    return n.bit_length() <= 248  # i.e. len(n.Bytes()) < 32 in Go


def _counting_hash_to_scalar(data: bytes) -> int:
    """The real derivation, plus a tally of how often the quirk mattered."""
    global _QUIRK_HITS
    if _reduction_is_short(data):
        _QUIRK_HITS += 1
    return _real_hash_to_scalar(data)


def _canonical_hash_to_scalar(data: bytes) -> int:
    """hash_to_scalar as a correct-by-instinct port would write it: right-aligned.

    This is the bug. It agrees with Go on 255 inputs out of 256, which is
    exactly why it is worth a negative control rather than an assumption.
    """
    global _QUIRK_HITS
    if _reduction_is_short(data):
        _QUIRK_HITS += 1
    return int.from_bytes(hashlib.sha3_512(data).digest(), "big") % sign.N


def message_for(index: int) -> bytes:
    """A distinct 32-byte message. Real relays sign a SHA-256 hash, so this does too."""
    return hashlib.sha256(f"pocket-relay-python-example-{index}".encode()).digest()


# --------------------------------------------------------------------------
# The oracle
# --------------------------------------------------------------------------


def run_oracle(args: List[str], stdin: Optional[str] = None) -> Tuple[int, str, str]:
    """Run the oracle, returning (exit code, stdout, stderr)."""
    proc = subprocess.run(
        [ORACLE_BIN, *args],
        input=stdin,
        capture_output=True,
        text=True,
    )
    return proc.returncode, proc.stdout, proc.stderr


def oracle_verify(msg: bytes, sig: bytes) -> Tuple[bool, str]:
    """Ask the real ring-go whether it accepts this signature. Exit 0 == accepted."""
    payload = json.dumps({"msg_hex": msg.hex(), "sig_hex": sig.hex()})
    code, out, err = run_oracle(["verify"], stdin=payload)
    if code == 0:
        return True, ""
    try:
        return False, json.loads(out).get("reason", out.strip())
    except json.JSONDecodeError:
        return False, (out + err).strip() or f"oracle exited {code}"


def oracle_verify_receipt(digest: bytes, sig: bytes, pub: bytes) -> Tuple[bool, str]:
    """Ask the real cosmos secp256k1 whether it accepts this receipt signature.

    This is what makes step 6 more than a self-consistency check. Our digest is
    computed in Python and then handed to the library the relayer signs with: if
    the two disagree about a single byte of the preimage, the library says no.
    """
    payload = json.dumps({"digest_hex": digest.hex(), "sig_hex": sig.hex(), "pub_hex": pub.hex()})
    code, out, err = run_oracle(["verify-receipt"], stdin=payload)
    if code == 0:
        return True, ""
    try:
        return False, json.loads(out).get("reason", out.strip())
    except json.JSONDecodeError:
        return False, (out + err).strip() or f"oracle exited {code}"


def oracle_response_vectors() -> dict:
    """A signed RelayResponse and receipt, built with the real poktroll types."""
    code, out, err = run_oracle(["response-vectors"])
    if code != 0:
        raise RuntimeError(f"oracle response-vectors exited {code}: {err.strip()}")
    return json.loads(out)


def _sign_and_check(index: int) -> Result:
    """Sign one message and hand it to the Go verifier. Runs in a pool worker."""
    global _QUIRK_HITS
    _QUIRK_HITS = 0

    msg = message_for(index)
    sig = sign.build_relay_signature(msg, RING, GW_PRIV, OUR_IDX)
    accepted, reason = oracle_verify(msg, sig)

    return Result(index, accepted, reason, _QUIRK_HITS, msg.hex(), sig.hex())


def _init_worker(oracle_bin: str, canonical: bool) -> None:
    """Set a pool worker up.

    Both globals have to be passed in rather than inherited: on macOS and
    Windows a worker is `spawn`ed, which re-imports this module fresh and would
    otherwise reset ORACLE_BIN to its default and lose the monkeypatch.
    """
    global ORACLE_BIN
    ORACLE_BIN = oracle_bin
    sign.hash_to_scalar = _canonical_hash_to_scalar if canonical else _counting_hash_to_scalar


def run_batch(count: int, jobs: int, canonical: bool) -> List[Result]:
    """Sign `count` messages in parallel and verify every one against the oracle."""
    if jobs == 1:
        _init_worker(ORACLE_BIN, canonical)
        return [_sign_and_check(i) for i in range(count)]

    with concurrent.futures.ProcessPoolExecutor(
        max_workers=jobs,
        initializer=_init_worker,
        initargs=(ORACLE_BIN, canonical),
    ) as pool:
        return list(pool.map(_sign_and_check, range(count), chunksize=8))


# --------------------------------------------------------------------------
# Checks
# --------------------------------------------------------------------------


class Step:
    """Collects the results of one numbered step so it can report a single verdict."""

    def __init__(self, heading: str) -> None:
        print(heading)
        self.ok = True

    def check(self, label: str, passed: bool, detail: str = "") -> bool:
        """Record one assertion. Returns what it was given, so it can be chained."""
        self.ok &= passed
        print(f"      {'pass' if passed else 'FAIL'}  {label}")
        if detail and not passed:
            print(f"            {detail}")
        return passed


def check_vectors() -> bool:
    """Compare our primitives to the oracle's deterministic vectors."""
    print("[1/6] deterministic vectors")

    code, out, err = run_oracle(["vectors"])
    if code != 0:
        print(f"      FAIL  oracle vectors exited {code}: {err.strip()}")
        return False
    vectors = json.loads(out)

    ok = True

    for v in vectors["hash_to_scalar"]:
        got = sign.encode_scalar(_real_hash_to_scalar(bytes.fromhex(v["input_hex"]))).hex()
        want = v["output_hex"]
        label = v.get("note", "")
        if got == want:
            print(f"      pass  hash_to_scalar   ({label[:52]})")
        else:
            ok = False
            print(f"      FAIL  hash_to_scalar   ({label})")
            print(f"            want {want}")
            print(f"            got  {got}")
            if "SHORT" in label:
                print(
                    "            This is the left-align quirk. Your reduction is\n"
                    "            probably right-aligned (zero-padded on the left).\n"
                    "            See hash_to_scalar() in sign.py."
                )

    for v in vectors["hash_to_curve"]:
        got = sign.compress_point(sign.hash_to_curve(bytes.fromhex(v["input_hex"]))).hex()
        if got == v["output_hex"]:
            print("      pass  hash_to_curve")
        else:
            ok = False
            print("      FAIL  hash_to_curve")
            print(f"            want {v['output_hex']}")
            print(f"            got  {got}")
            print(
                "            Check: SHA3-256 (not Keccak), EVEN y, and re-hash\n"
                "            the DIGEST on a miss (not the public key)."
            )

    for v in vectors["scalar_encoding"]:
        got = sign.encode_scalar(int.from_bytes(bytes.fromhex(v["input_hex"]), "big")).hex()
        if got == v["output_hex"]:
            # Same sentence as the Node harness prints, for the same check.
            # The two halves are one fact stated twice: the VALUE sits at the
            # right-hand end of the 32 bytes, which is the same as saying the
            # PADDING goes on the left. Say both, because in this directory
            # "left" and "right" are the difference between a working port and
            # one that drops a relay in every 128.
            print(
                "      pass  scalar_encoding is right-aligned on the wire — the value at\n"
                "            the right end, zero padding on the LEFT (the opposite of\n"
                "            hash_to_scalar)"
            )
        else:
            ok = False
            print(f"      FAIL  scalar_encoding  want {v['output_hex']} got {got}")

    keys = vectors["keys"]
    for label, priv, want in (
        ("app", keys["app_priv_hex"], keys["app_pub_hex"]),
        ("gateway", keys["gateway_priv_hex"], keys["gateway_pub_hex"]),
    ):
        got = sign.public_key_from_private(bytes.fromhex(priv)).hex()
        if got == want:
            print(f"      pass  pubkey derivation ({label})")
        else:
            ok = False
            print(f"      FAIL  pubkey derivation ({label}): want {want} got {got}")

    return ok


def check_go_signature() -> bool:
    """Our verifier must accept a real Go signature -- and must be able to say no.

    Accepting is only half a test. A verifier hardcoded to `return True` passes
    an accept-only check, and sign.py ships a verifier readers copy, so the
    rejections below are what make the acceptance above mean anything.
    """
    step = Step("\n[2/6] go -> python: our verifier on a real ring-go signature")

    code, out, err = run_oracle(["sign"])
    if code != 0:
        print(f"      FAIL  oracle sign exited {code}: {err.strip()}")
        return False

    got = json.loads(out)
    msg = bytes.fromhex(got["msg_hex"])
    sig = bytes.fromhex(got["sig_hex"])

    step.check(
        f"accepts a {len(sig)}-byte Go signature",
        sign.verify_relay_signature(msg, sig),
        f"msg_hex {got['msg_hex']}\n            sig_hex {got['sig_hex']}",
    )

    # Tamper with the two scalars in the wire layout
    # [4B ring size][32B c[0]][33B key image][n x (32B s_i || 33B P_i)].
    # Both stay structurally valid when a byte flips -- any 32 bytes is a
    # scalar -- so the ONLY thing that can reject them is the challenge loop
    # failing to close. Flipping a point instead would prove much less: it
    # would fail at decompression, which is parsing, not verification.
    c0_edit = bytearray(sig)
    c0_edit[4] ^= 0x01
    step.check("rejects a tampered c[0]", not sign.verify_relay_signature(msg, bytes(c0_edit)))

    s0_edit = bytearray(sig)
    s0_edit[sign.SIGNATURE_HEADER_SIZE] ^= 0x01
    step.check("rejects a tampered s[0]", not sign.verify_relay_signature(msg, bytes(s0_edit)))

    other_msg = hashlib.sha256(b"a different relay entirely").digest()
    step.check(
        "rejects the right signature over the wrong message",
        not sign.verify_relay_signature(other_msg, sig),
    )

    # sign.py promises that any bytes at all are safe to hand this function:
    # a signature comes off the network, so malformed is an answer and not an
    # exception. Hold it to that.
    malformed = {
        "empty": b"",
        "truncated": sig[:100],
        "one byte too long": sig + b"\x00",
        "a ring size of 0": (0).to_bytes(4, "big") + sig[4:],
        "an absurd ring size": (0xFFFFFFFF).to_bytes(4, "big") + sig[4:],
        # 0x04 is an uncompressed SEC1 prefix, which decompress_point refuses.
        # This is the case that would raise if the try/except came out.
        "an uncompressed ring member": sig[: -sign.SIGNATURE_MEMBER_SIZE + 32]
        + b"\x04"
        + sig[-32:],
    }
    for label, bad in malformed.items():
        try:
            rejected = not sign.verify_relay_signature(msg, bad)
        except Exception as exc:  # noqa: BLE001 -- catching is the point of the check
            step.check(f"returns False for {label}", False, f"raised {type(exc).__name__}: {exc}")
        else:
            step.check(f"returns False for {label}", rejected)

    return step.ok


def check_signatures(count: int, jobs: int) -> bool:
    """The real test: `count` signatures, every one accepted by ring-go."""
    print(f"\n[3/6] python -> go: {count} signatures, each verified by real ring-go")

    results = run_batch(count, jobs, canonical=False)

    accepted = sum(1 for r in results if r.accepted)
    quirk_hits = sum(r.quirk_hits for r in results)
    failures = [r for r in results if not r.accepted]

    print(f"      accepted     {accepted}/{count}")
    print(
        f"      quirk fired  {quirk_hits} times in {count * len(RING)} challenges "
        f"(~{quirk_hits / max(count * len(RING), 1) * 100:.2f}%, expected ~0.39%)"
    )

    ok = True

    if failures:
        ok = False
        print(f"      FAIL  {len(failures)} signature(s) rejected")
        rate = len(failures) / count * 100
        print(f"            rejection rate {rate:.2f}%")
        if rate < 3.0:
            print(
                "            A small-but-nonzero rate is the signature of the\n"
                "            left-align quirk being missed. See hash_to_scalar()."
            )
        for r in failures[:3]:
            print(f"            [{r.index}] {r.reason}")
            print(f"                  msg_hex {r.msg_hex}")
            print(f"                  sig_hex {r.sig_hex}")
    else:
        print("      pass  100% accepted by ring-go")

    # A run that never tripped the quirk hasn't tested it. At 2 challenges per
    # signature this needs ~128 signatures to be likely; complain rather than
    # let it pass quietly and be mistaken for evidence.
    if quirk_hits == 0:
        ok = False
        print(
            f"      FAIL  the quirk never fired in {count} signatures, so this run\n"
            f"            did not test it. Raise --count (>=1500 is the floor)."
        )

    return ok


def check_negative_control(count: int, jobs: int) -> bool:
    """Prove the test has teeth: a canonical hash_to_scalar must get rejected.

    Mirrors the oracle's own TestCanonicalHashToScalarIsRejected. If this
    passes, the harness above is capable of detecting a wrong implementation --
    which is the whole reason to trust its 100%.
    """
    print(f"\n[4/6] negative control: {count} signatures with the quirk removed")

    results = run_batch(count, jobs, canonical=True)

    rejected = sum(1 for r in results if not r.accepted)
    rate = rejected / count * 100
    print(f"      rejected     {rejected}/{count} ({rate:.2f}%, expected ~0.78%)")

    if rejected == 0:
        print(
            "      FAIL  a canonical (right-aligning) hash_to_scalar was never\n"
            "            rejected. Either this harness is not really reaching\n"
            "            ring-go, or go-dleq changed. Until this is understood,\n"
            "            the 100% above is not evidence of anything."
        )
        return False

    if rate > 5.0:
        print(
            f"      FAIL  rejection rate {rate:.2f}% is far above the ~0.78% the\n"
            "            quirk explains, so something broader is wrong."
        )
        return False

    print("      pass  the quirk is load-bearing, and this harness detects its absence")
    return True


def check_response(vectors: dict) -> bool:
    """verify.py must agree with the relayer about what a response says."""
    step = Step("\n[5/6] response: signable bytes, signature, and the payload binding")

    supplier_pub = bytes.fromhex(vectors["supplier"]["pub_hex"])
    relay_response_bz = bytes.fromhex(vectors["relay_response_hex"])
    response = verify.parse_relay_response(relay_response_bz)

    # The bytes are the whole game. If these match, the hash matches, and the
    # signature check downstream is arithmetic rather than guesswork.
    signable = verify.filter_signable_bytes(relay_response_bz)
    step.check(
        "filtered signable bytes are byte-identical to the oracle's",
        signable.hex() == vectors["signable_bytes_hex"],
        f"want {vectors['signable_bytes_hex']}\n            got  {signable.hex()}",
    )
    step.check(
        "the supplier signature and the payload are the only things filtered out",
        response.supplier_operator_signature not in signable and response.payload not in signable,
    )
    step.check(
        "payload_hash survives the filter (it is what stands in for the payload)",
        response.payload_hash in signable,
    )

    # The double hash, pinned against the oracle instead of inferred. A port
    # that hashes once still fails the signature check below, but it fails with
    # no clue as to why; this line names the mistake.
    ecdsa_hash = verify.ecdsa_message_hash(verify.response_signable_hash(relay_response_bz))
    step.check(
        "the hash raw ECDSA covers is sha256(sha256(signable bytes))",
        ecdsa_hash.hex() == vectors["response_ecdsa_message_hash_hex"],
        f"want {vectors['response_ecdsa_message_hash_hex']}\n            got  {ecdsa_hash.hex()}",
    )
    step.check(
        "...which is NOT the digest handed to cosmos -- the two must differ",
        ecdsa_hash != verify.response_signable_hash(relay_response_bz),
    )

    opened, reason = verify.open_relay_response(relay_response_bz, supplier_pub)
    step.check("the supplier operator signature verifies", opened is not None, reason)

    # Two different mechanisms guard two different parts of the response, and a
    # port can easily implement one and quietly skip the other. Break each in
    # turn: an edited payload must be caught by the hash binding, an edited
    # session header by the signature.
    def flip_a_bit(needle: bytes) -> bytes:
        """The response with one bit of `needle` inverted, wherever it sits."""
        at = relay_response_bz.find(needle)
        if at < 0:
            raise AssertionError(f"{needle!r} is not in the oracle's response")
        edited = bytearray(relay_response_bz)
        edited[at] ^= 0x01
        return bytes(edited)

    assert response.session_header is not None, "the oracle's response has no session header"
    step.check(
        "an edited payload is rejected (the payload_hash binding catches it)",
        verify.open_relay_response(flip_a_bit(response.payload), supplier_pub)[0] is None,
        "the payload is NOT signed, only its hash is -- a verifier that skips\n"
        "            the re-hash accepts any body an intermediary cares to swap in",
    )
    step.check(
        "an edited session header is rejected (the signature catches it)",
        verify.open_relay_response(
            flip_a_bit(response.session_header.service_id.encode()), supplier_pub
        )[0]
        is None,
    )
    step.check(
        "the wrong public key is rejected",
        verify.open_relay_response(relay_response_bz, RING[0])[0] is None,
    )

    # Records smuggled in on top of a genuine response. Because the signable
    # bytes are FILTERED rather than rebuilt, every one of these is inside the
    # hash, so none of them can be signed. A verifier that re-encoded from
    # parsed structs would drop them and happily accept the message.
    appended = {
        "a duplicated payload_hash": bytes.fromhex("2220" + vectors["payload_hash_hex"]),
        "a second payload_hash for another response": bytes.fromhex(
            "2220" + vectors["negative_controls"]["other_payload_hash_hex"]
        ),
        "an unknown field": b"\x4a\x03abc",
    }
    for label, extra in appended.items():
        step.check(
            f"{label} appended to the response is rejected",
            verify.open_relay_response(relay_response_bz + extra, supplier_pub)[0] is None,
        )

    # The backwards-compatibility branch in filter_signable_bytes: a relayer
    # older than v0.1.25 sends no payload_hash, and for those the payload IS
    # part of the signed bytes. Constructed by removing field 4 from the
    # oracle's own bytes, so it is the real encoding minus one record. The
    # signature cannot be checked -- nobody signed this -- but the filtering
    # branch can, and an unconditional drop would make every such response look
    # forged.
    payload_hash_record = bytes.fromhex("2220" + vectors["payload_hash_hex"])
    if step.check(
        "the oracle's response ends with the payload_hash record",
        relay_response_bz.endswith(payload_hash_record),
    ):
        legacy = relay_response_bz[: -len(payload_hash_record)]
        legacy_signable = verify.filter_signable_bytes(legacy)
        step.check(
            "with no payload_hash, the payload stays in the signable bytes",
            response.payload in legacy_signable,
        )
        step.check(
            "...and the supplier signature still comes out",
            response.supplier_operator_signature not in legacy_signable,
        )

    # The answer a caller actually wanted, two layers down.
    inner = vectors["inner_pokt_http_response"]
    http = verify.parse_pokt_http_response(response.payload)
    step.check(
        "the inner POKTHTTPResponse parses to the oracle's status, headers and body",
        (
            http.status_code == inner["status_code"]
            and http.headers == inner["headers"]
            and http.body.hex() == inner["body_bz_hex"]
        ),
        f"status {http.status_code} headers {http.headers} body {http.body!r}",
    )
    # protobuf has no map on the wire and no ordering guarantee for one, and a
    # single-header fixture cannot tell a working map parser from a lucky one.
    # Name the two properties the fixture now exercises so a regression in
    # either is legible instead of hiding inside the dict comparison above.
    repeated = [name for name, values in http.headers.items() if len(values) > 1]
    step.check(
        "a header with several values keeps all of them, in order",
        bool(repeated) and all(http.headers[name] == inner["headers"][name] for name in repeated),
        f"headers with >1 value: {repeated or 'none -- the fixture stopped covering this'}",
    )
    step.check("every header in the fixture is parsed", len(http.headers) == len(inner["headers"]))

    return step.ok


def check_receipt(vectors: dict) -> bool:
    """The receipt must verify, must fail on either negative control, and cosmos must agree."""
    step = Step("\n[6/6] receipt: bound to one request and one response, judged by cosmos")

    supplier_pub = bytes.fromhex(vectors["supplier"]["pub_hex"])
    receipt = vectors["receipt"]
    controls = vectors["negative_controls"]

    request_sig = bytes.fromhex(receipt["request_signature_hex"])
    payload_hash = bytes.fromhex(vectors["payload_hash_hex"])
    receipt_sig = bytes.fromhex(receipt["signature_hex"])
    header_value = receipt["header_value"]

    step.check(
        "the domain tag is the 22 bytes the relayer uses",
        verify.RECEIPT_DOMAIN_TAG.hex() == receipt["domain_tag_hex"],
        f"want {receipt['domain_tag_hex']}\n            got  {verify.RECEIPT_DOMAIN_TAG.hex()}",
    )

    digest = verify.receipt_digest(request_sig, payload_hash)
    step.check(
        "our digest is byte-identical to the oracle's",
        digest.hex() == receipt["digest_hex"],
        f"want {receipt['digest_hex']}\n            got  {digest.hex()}",
    )
    receipt_ecdsa_hash = verify.ecdsa_message_hash(digest)
    step.check(
        "the hash raw ECDSA covers is sha256 of that digest again",
        receipt_ecdsa_hash.hex() == receipt["ecdsa_message_hash_hex"],
        f"want {receipt['ecdsa_message_hash_hex']}\n            got  {receipt_ecdsa_hash.hex()}",
    )
    step.check(
        "the header parses to the 64-byte signature, in either case",
        verify.parse_receipt_header(header_value) == receipt_sig
        and verify.parse_receipt_header("v1." + header_value[3:].upper()) == receipt_sig,
    )
    step.check(
        "the receipt verifies",
        verify.verify_receipt(header_value, request_sig, payload_hash, supplier_pub),
    )

    # The negative controls. A receipt that verifies is worth nothing on its own
    # -- what it claims is that this response answers THIS request, so it has to
    # fail when either half of the pair is replaced.
    other_hash = bytes.fromhex(controls["other_payload_hash_hex"])
    other_request_sig = bytes.fromhex(controls["other_request_signature_hex"])
    step.check(
        "it FAILS for a different response (same request)",
        not verify.verify_receipt(header_value, request_sig, other_hash, supplier_pub),
    )
    step.check(
        "it FAILS for a different request (same response)",
        not verify.verify_receipt(header_value, other_request_sig, payload_hash, supplier_pub),
    )
    step.check(
        "it FAILS for the wrong public key",
        not verify.verify_receipt(header_value, request_sig, payload_hash, RING[0]),
    )
    malformed = ["v2." + receipt_sig.hex(), "v1.zz", "v1." + receipt_sig.hex()[:-2], ""]
    step.check(
        "a malformed header is rejected rather than raising",
        not any(
            verify.verify_receipt(value, request_sig, payload_hash, supplier_pub)
            for value in malformed
        ),
    )

    # Low-S. (r, N-s) is as mathematically valid as (r, s), so the ONLY reason
    # to reject it is the rule cosmos enforces. Prove both halves of that here:
    # the bare curve arithmetic accepts it, and the verifier does not. A port
    # that omits the rule is more permissive than the chain, which is the one
    # direction in which a verifier must never be wrong.
    negated_s = (sign.N - int.from_bytes(receipt_sig[32:], "big")) % sign.N
    high_s = receipt_sig[:32] + negated_s.to_bytes(32, "big")
    step.check(
        "negating S produces a signature the curve arithmetic still accepts",
        _verify_ignoring_low_s(supplier_pub, digest, high_s),
    )
    step.check(
        "...and we reject it anyway, because cosmos does",
        not verify.verify_cosmos_signature(supplier_pub, digest, high_s),
    )

    # Everything above is our own maths agreeing with itself. Now ask the
    # library the relayer actually signs with.
    print("      ---- cross-checked against cosmos secp256k1 in the oracle ----")
    accepted, reason = oracle_verify_receipt(digest, receipt_sig, supplier_pub)
    step.check("cosmos accepts the digest WE computed", accepted, reason)

    # The double hash from the other side. cosmos wants the DIGEST and hashes it
    # itself; hand it the already-hashed value and it hashes a third time and
    # says no. This is the same trap that makes a raw ECDSA library reject the
    # digest, and running it both ways is what keeps the direction straight.
    accepted, _ = oracle_verify_receipt(receipt_ecdsa_hash, receipt_sig, supplier_pub)
    step.check("cosmos REJECTS the already-hashed ECDSA message (it hashes again)", not accepted)

    for label, bad_digest in (
        ("a different response", verify.receipt_digest(request_sig, other_hash)),
        ("a different request", verify.receipt_digest(other_request_sig, payload_hash)),
    ):
        accepted, _ = oracle_verify_receipt(bad_digest, receipt_sig, supplier_pub)
        step.check(f"cosmos rejects the digest for {label}", not accepted)

    accepted, _ = oracle_verify_receipt(digest, high_s, supplier_pub)
    step.check("cosmos rejects the high-S signature too", not accepted)

    return step.ok


def _verify_ignoring_low_s(pubkey_bz: bytes, signed_bytes: bytes, signature: bytes) -> bool:
    """ECDSA verification with the low-S rule deliberately left out.

    Only exists so the low-S check above can be shown to be the thing doing the
    rejecting, rather than some unrelated malformation. Never do this for real.
    """
    pubkey = sign.decompress_point(pubkey_bz)
    r = int.from_bytes(signature[:32], "big")
    s = int.from_bytes(signature[32:], "big")
    e = int.from_bytes(hashlib.sha256(signed_bytes).digest(), "big") % sign.N
    w = pow(s, -1, sign.N)
    point = sign.point_add(
        sign.scalar_base_mult(e * w % sign.N), sign.scalar_mult(r * w % sign.N, pubkey)
    )
    return point is not None and point[0] % sign.N == r


# --------------------------------------------------------------------------
# main
# --------------------------------------------------------------------------


def find_oracle(explicit: Optional[str]) -> str:
    """Locate the oracle binary, or explain how to build it."""
    here = Path(__file__).resolve().parent
    candidates = [
        explicit,
        os.environ.get("ORACLE"),
        "/tmp/oracle",  # what ../README.md tells you to build
        str(here / "oracle-bin"),  # a local build; gitignored under this name
        shutil.which("oracle"),
    ]
    for c in candidates:
        if c and Path(c).is_file() and os.access(c, os.X_OK):
            return str(Path(c).resolve())

    print(
        "cannot find the oracle binary. Build it with:\n\n"
        f'    go build -o /tmp/oracle "{here.parent}/oracle/"\n\n'
        "or point at an existing one with --oracle PATH.\n"
        "(Building it into this directory as ./oracle or ./oracle-bin also works;\n"
        "both names are gitignored, so the binary cannot be committed.)",
        file=sys.stderr,
    )
    sys.exit(2)


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Test sign.py against the real Go ring-go verifier.",
    )
    parser.add_argument(
        "--count",
        type=int,
        default=1500,
        help="signatures to verify (default 1500; below ~1500 cannot see the quirk)",
    )
    parser.add_argument("--oracle", help="path to the oracle binary")
    parser.add_argument(
        "--jobs",
        type=int,
        default=min(os.cpu_count() or 1, 16),
        help="parallel workers (default: cpu count, capped at 16; use 1 to debug)",
    )
    parser.add_argument(
        "--skip-negative-control",
        action="store_true",
        help=(
            "skip step 4, the slow negative control (it is what proves steps 1-3 can "
            "fail; skip only to save time). Steps 5 and 6 carry their own negative "
            "controls and always run -- they are instant."
        ),
    )
    args = parser.parse_args()

    global ORACLE_BIN
    ORACLE_BIN = find_oracle(args.oracle)

    print(f"oracle  {ORACLE_BIN}")
    print(f"ring    [app, gateway], signer index {OUR_IDX} (the gateway)")
    print(f"jobs    {args.jobs}\n")

    if args.count < 1500:
        print(
            f"warning: --count {args.count} is below the 1500 floor. The quirk fires\n"
            f"         ~1 signature in 128; a small run passes for a wrong port.\n"
        )

    ok = check_vectors()
    ok &= check_go_signature()
    ok &= check_signatures(args.count, args.jobs)
    if not args.skip_negative_control:
        ok &= check_negative_control(args.count, args.jobs)

    # Steps 1-4 leave sign.hash_to_scalar monkeypatched when --jobs 1. Nothing
    # in the return trip uses it, but restore it anyway rather than leave a
    # patched module for whatever runs next.
    sign.hash_to_scalar = _real_hash_to_scalar

    # One document, shared by both return-trip steps: they check two halves of
    # the same received response, which is how a caller meets them.
    vectors = oracle_response_vectors()
    ok &= check_response(vectors)
    ok &= check_receipt(vectors)

    print(
        "\n"
        + (
            "PASS: ring-go accepts every signature sign.py produces, and cosmos\n"
            "      agrees with every verdict verify.py reaches."
            if ok
            else "FAIL"
        )
    )
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
