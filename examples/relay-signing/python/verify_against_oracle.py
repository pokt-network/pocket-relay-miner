#!/usr/bin/env python3
"""Test sign.py against the real Go verifier.

Round-tripping your own sign->verify proves nothing: an implementation can be
perfectly self-consistent and still be rejected by every relayer it ever talks
to. The only authority is github.com/pokt-network/ring-go, which is what the
oracle in ../oracle wraps. So every signature made here is handed to it.

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

Four things are checked, in increasing order of how much they prove:

  1. vectors        -- the deterministic primitives, so a failure below can be
                       bisected to the exact function that drifted
  2. go -> python   -- our verifier accepts a signature made by real Go
  3. python -> go   -- real Go accepts `--count` of our signatures. 100%, no
                       retries. Also reports how often the quirk actually
                       fired, because a test that never hit it proved nothing
  4. negative ctrl  -- the same signatures, but with the quirk deliberately
                       removed, must get REJECTED. This is what shows the test
                       has teeth: without it, a vacuous harness that accepts
                       everything looks identical to a passing one
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

# The oracle's throwaway test keys (examples/relay-signing/oracle/main.go).
APP_PRIV = bytes.fromhex("c188c43496351a963762a5d9de78ff887ac66b4ba5de5967efd55a6d1e71ddda")
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


def check_vectors() -> bool:
    """Compare our primitives to the oracle's deterministic vectors."""
    print("[1/4] deterministic vectors")

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
            print("      pass  scalar_encoding  (canonical: zero-padded on the LEFT)")
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
    """Our verifier must accept a signature produced by the real Go signer."""
    print("\n[2/4] go -> python: our verifier on a real ring-go signature")

    code, out, err = run_oracle(["sign"])
    if code != 0:
        print(f"      FAIL  oracle sign exited {code}: {err.strip()}")
        return False

    got = json.loads(out)
    msg = bytes.fromhex(got["msg_hex"])
    sig = bytes.fromhex(got["sig_hex"])

    if sign.verify_relay_signature(msg, sig):
        print(f"      pass  accepted a {len(sig)}-byte Go signature")
        return True

    print("      FAIL  our verifier rejected a signature the Go signer produced")
    print(f"            msg_hex {got['msg_hex']}")
    print(f"            sig_hex {got['sig_hex']}")
    return False


def check_signatures(count: int, jobs: int) -> bool:
    """The real test: `count` signatures, every one accepted by ring-go."""
    print(f"\n[3/4] python -> go: {count} signatures, each verified by real ring-go")

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
    print(f"\n[4/4] negative control: {count} signatures with the quirk removed")

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
        help="skip step 4 (it is what proves the test can fail; skip only to save time)",
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

    print("\n" + ("PASS: ring-go accepts every signature sign.py produces." if ok else "FAIL"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
