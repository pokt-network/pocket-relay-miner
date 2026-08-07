//! Test harness: sign a lot of relays, and make the Go oracle judge every one.
//!
//! The point of signing *a lot* is [`pocket_relay_signing::hash_to_scalar`]'s
//! left-align quirk. It fires on ~1 challenge in 256, so with the 2-member
//! `[app, gateway]` ring a wrong implementation is rejected on only ~1
//! signature in 128. A 10-signature test therefore passes for a broken signer
//! ~93% of the time, and a 100-signature test still passes ~45% of the time.
//! At 1500 signatures a canonical implementation is expected to fail ~12 times
//! and passes by luck with probability ~1e-5.
//!
//! So: 1500 signatures, 100% acceptance required. If you see a handful of
//! failures out of 1500, do not retry the run -- that IS the bug.
//!
//! Usage (build the oracle first: `go build -o /tmp/oracle ../oracle/`):
//!
//!     cargo run --release -- /tmp/oracle                     # 1500 signatures
//!     cargo run --release -- /tmp/oracle --negative-control  # prove it can fail
//!     cargo run --release -- /tmp/oracle --response          # the return trip
//!
//! An optional count before the flag overrides the 1500 default, but a smaller
//! run cannot tell a correct signer from a broken one -- so if the quirk never
//! fires, this harness reports INCONCLUSIVE and exits non-zero rather than
//! calling that a pass.
//!
//! `--negative-control` signs with the canonical (wrong) derivation instead,
//! and *requires* the oracle to reject some. It proves this harness can
//! actually detect the bug it exists to detect -- a test suite that has never
//! failed is not evidence of anything.
//!
//! `--response` checks the other direction with `pocket_relay_signing::verify`:
//! a freshly generated, Go-signed RelayResponse and receipt from
//! `oracle response-vectors`. It matters that the vectors are fresh -- the
//! receipt binds a bLSAG signature, which is randomised, so the unit tests can
//! only pin one captured run. This exercises whatever the oracle produces
//! today, and hands our own recomputed digest back to `oracle verify-receipt`
//! so the real cosmos verifier gets the last word.

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode, Stdio};

use k256::elliptic_curve::ops::Reduce;
use k256::elliptic_curve::scalar::IsHigh;
use k256::elliptic_curve::{bigint::Encoding, Generate, PrimeField};
use k256::{PublicKey, Scalar, SecretKey, U256};
use pocket_relay_signing::verify::{self, VerifyError};
use pocket_relay_signing::{
    build_relay_signature_canonical, build_relay_signature_with_stats, SignStats,
};

/// Throwaway test keys, matching `oracle vectors`. Not used to hold anything.
const APP_PRIV_HEX: &str = "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a";
const GW_PRIV_HEX: &str = "1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11";

/// A Pocket ring is `[app, gateway]`, in that order, unsorted. The gateway
/// signs, so it is at index 1.
const GATEWAY_IDX: usize = 1;

const DEFAULT_COUNT: u32 = 1500;

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().collect();
    let Some(oracle) = args.get(1).map(PathBuf::from) else {
        eprintln!(
            "usage: {} <path-to-oracle> [count] [--negative-control | --response]",
            args[0]
        );
        return ExitCode::from(2);
    };
    if args.iter().any(|a| a == "--response") {
        return run_response_checks(&oracle);
    }
    let count: u32 = args
        .get(2)
        .filter(|a| !a.starts_with("--"))
        .map_or(Ok(DEFAULT_COUNT), |a| a.parse())
        .unwrap_or_else(|e| {
            eprintln!("bad count: {e}");
            std::process::exit(2);
        });
    let negative_control = args.iter().any(|a| a == "--negative-control");

    let app_key = secret_from_hex(APP_PRIV_HEX);
    let gw_key = secret_from_hex(GW_PRIV_HEX);
    let ring = [app_key.public_key(), gw_key.public_key()];

    if negative_control {
        return run_negative_control(&oracle, count, &ring, &gw_key);
    }

    match run_pocket_ring(&oracle, count, &ring, &gw_key) {
        Ok(()) => {}
        Err(code) => return code,
    }
    run_larger_rings(&oracle)
}

/// The real thing: the `[app, gateway]` ring, signed by the gateway, 1500 times.
fn run_pocket_ring(
    oracle: &Path,
    count: u32,
    ring: &[PublicKey],
    gw_key: &SecretKey,
) -> Result<(), ExitCode> {
    println!(
        "signing {count} relays with the [app, gateway] ring (gateway at index {GATEWAY_IDX})"
    );

    let mut stats = SignStats::default();
    let mut rejected: Vec<(u32, String)> = Vec::new();

    for i in 0..count {
        let m = relay_hash(i);
        let (sig, sig_stats) = build_relay_signature_with_stats(&m, ring, gw_key, GATEWAY_IDX)
            .unwrap_or_else(|e| fatal(&format!("signing message {i}: {e}")));
        stats.add(sig_stats);

        if sig.len() != 199 {
            fatal(&format!(
                "message {i}: signature is {} bytes, want 199",
                sig.len()
            ));
        }

        let verdict = ask_oracle(oracle, &m, &sig);
        if !verdict.accepted {
            rejected.push((i, verdict.detail));
        }

        if (i + 1) % 250 == 0 {
            println!(
                "  {:>5}/{count} signed, {} accepted, {} short reductions so far",
                i + 1,
                i + 1 - rejected.len() as u32,
                stats.short_reductions,
            );
        }
    }

    let accepted = count - rejected.len() as u32;
    println!("\n--- results ---");
    println!("accepted:         {accepted}/{count}");
    report_quirk(&stats);

    if !rejected.is_empty() {
        println!("\nREJECTED {} of {count}:", rejected.len());
        for (i, detail) in rejected.iter().take(5) {
            println!("  message {i}: {detail}");
        }
        let rate = rejected.len() as f64 / f64::from(count);
        println!(
            "\nrejection rate {:.2}%. ~0.78% means hash_to_scalar is not reproducing the\n\
             left-align quirk (see lib.rs). Anything higher is a deeper divergence:\n\
             bisect against `oracle vectors` before touching the signer.",
            rate * 100.0,
        );
        return Err(ExitCode::FAILURE);
    }

    // A run where the quirk never fired has not tested the quirk, so every
    // signature could have been produced by a canonical implementation too --
    // "all accepted" would mean nothing. Whether that is *suspicious* depends
    // on the sample: P(zero) = (255/256)^challenges is ~8e-6 over the default
    // 1500 signatures but ~92% over 10, so only fail when zero is genuinely
    // surprising.
    let p_zero = (255.0f64 / 256.0).powf(stats.challenges as f64);
    if stats.short_reductions == 0 {
        if p_zero < 0.001 {
            println!(
                "\nFAIL: {count} signatures were accepted but the quirk never fired.\n\
                 P(that happening by chance) = {p_zero:.1e}. Far more likely, hash_to_scalar\n\
                 is not reducing mod N correctly, and this run did not test what it claims.",
            );
            return Err(ExitCode::FAILURE);
        }
        println!(
            "\nINCONCLUSIVE: the quirk never fired, but at {count} signatures that is\n\
             unremarkable (p = {:.0}%). This run does NOT distinguish a correct signer\n\
             from a canonical one -- rerun with at least {DEFAULT_COUNT}.",
            p_zero * 100.0,
        );
        return Err(ExitCode::FAILURE);
    }

    println!("\nall {count} signatures accepted by the Go oracle (ring-go).");
    Ok(())
}

/// bLSAG is not specific to 2-member rings, and `(our_idx + i) % size` is easy
/// to get subtly wrong in a way `n = 2` hides. The oracle verifies whatever
/// ring is embedded in the signature, so this costs nothing to check.
fn run_larger_rings(oracle: &Path) -> ExitCode {
    println!("\nsigning larger rings from every position");

    for size in [3usize, 5] {
        let keys: Vec<SecretKey> = (0..size).map(|_| SecretKey::generate()).collect();
        let ring: Vec<PublicKey> = keys.iter().map(SecretKey::public_key).collect();

        for (our_idx, key) in keys.iter().enumerate() {
            for i in 0..10u32 {
                let m = relay_hash(i);
                let sig = build_relay_signature_with_stats(&m, &ring, key, our_idx)
                    .unwrap_or_else(|e| fatal(&format!("signing ring of {size}: {e}")))
                    .0;

                if sig.len() != 69 + 65 * size {
                    fatal(&format!("ring of {size}: signature is {} bytes", sig.len()));
                }
                let verdict = ask_oracle(oracle, &m, &sig);
                if !verdict.accepted {
                    println!(
                        "  FAIL ring of {size}, signer {our_idx}: {}",
                        verdict.detail
                    );
                    return ExitCode::FAILURE;
                }
            }
        }
        println!("  ring of {size}: 10 signatures from each of {size} positions, all accepted");
    }

    ExitCode::SUCCESS
}

/// Signs with the canonical (right-aligning) derivation, which is what a
/// reasonable port writes, and requires the oracle to reject some of them.
///
/// A test that cannot fail is not a test. This is the proof that the 1500-run
/// above is load-bearing: same harness, same oracle, one primitive changed.
fn run_negative_control(
    oracle: &Path,
    count: u32,
    ring: &[PublicKey],
    gw_key: &SecretKey,
) -> ExitCode {
    println!("NEGATIVE CONTROL: signing {count} relays with the canonical (WRONG) hash_to_scalar");
    println!("expecting the oracle to reject ~0.78% of them (1 - (255/256)^2)\n");

    let mut rejected = 0u32;
    for i in 0..count {
        let m = relay_hash(i);
        let (sig, _) = build_relay_signature_canonical(&m, ring, gw_key, GATEWAY_IDX)
            .unwrap_or_else(|e| fatal(&format!("signing message {i}: {e}")));
        if !ask_oracle(oracle, &m, &sig).accepted {
            rejected += 1;
        }
    }

    let rate = f64::from(rejected) / f64::from(count);
    println!(
        "rejected: {rejected}/{count} = {:.2}% (predicted ~0.78%)",
        rate * 100.0
    );

    if rejected == 0 {
        println!(
            "\nFAIL: the canonical derivation was never rejected. Either the harness is\n\
             not reaching ring-go, or go-dleq fixed the left-align -- in which case\n\
             hash_to_scalar and its docs must change together, and every existing port\n\
             breaks.",
        );
        return ExitCode::FAILURE;
    }

    println!("\ngood: the harness detects the bug it exists to detect.");
    ExitCode::SUCCESS
}

// ---------------------------------------------------------------------------
// The return trip: response signature and receipt
// ---------------------------------------------------------------------------

/// Checks a freshly generated RelayResponse and receipt, in both directions.
///
/// Every positive check has a negative twin. The receipt's whole claim is "this
/// response answers THAT request", and a verifier that accepts a receipt from
/// another relay -- or another response -- makes exactly the same noise as one
/// that works.
fn run_response_checks(oracle: &Path) -> ExitCode {
    println!("verifying a fresh RelayResponse and receipt from `oracle response-vectors`\n");

    let vectors = run_oracle(oracle, "response-vectors", None);
    if !vectors.accepted {
        fatal(&format!(
            "`oracle response-vectors` failed: {}",
            vectors.detail
        ));
    }
    let json = &vectors.detail;

    let supplier_pubkey = json_hex(json, "pub_hex");
    let relay_response = json_hex(json, "relay_response_hex");
    let want_signable = json_hex(json, "signable_bytes_hex");
    let want_payload_hash = json_hex(json, "payload_hash_hex");
    let want_body = json_hex(json, "body_bz_hex");
    let request_signature = json_hex(json, "request_signature_hex");
    let want_digest = json_hex(json, "digest_hex");
    let receipt_header = json_field(json, "header_value");
    let other_payload_hash = json_hex(json, "other_payload_hash_hex");
    let other_request_signature = json_hex(json, "other_request_signature_hex");

    let mut checks = Checks::default();

    // -- the response --------------------------------------------------
    println!("response");
    match verify::signable_bytes(&relay_response) {
        Ok(signable) => checks.equal(
            "filtered signable bytes match the oracle",
            &signable,
            &want_signable,
        ),
        Err(e) => checks.fail("filtered signable bytes match the oracle", &e.to_string()),
    }
    checks.accepts(
        "supplier operator signature verifies",
        verify::verify_response_signature(&relay_response, &supplier_pubkey),
    );
    match verify::payload_hash(&relay_response) {
        Ok(hash) => checks.equal("payload hash matches the oracle", hash, &want_payload_hash),
        Err(e) => checks.fail("payload hash matches the oracle", &e.to_string()),
    }

    // -- the receipt ---------------------------------------------------
    println!("\nreceipt");
    let digest = match verify::receipt_digest(&request_signature, &want_payload_hash) {
        Ok(digest) => {
            checks.equal("receipt digest matches the oracle", &digest, &want_digest);
            digest
        }
        Err(e) => {
            checks.fail("receipt digest matches the oracle", &e.to_string());
            return checks.finish();
        }
    };
    checks.accepts(
        "receipt verifies",
        verify::verify_receipt(
            &receipt_header,
            &request_signature,
            &want_payload_hash,
            &supplier_pubkey,
        ),
    );

    // The reverse direction. Our digest, the oracle's cosmos verifier: this is
    // the check that our two hashes are the two hashes cosmos expects, rather
    // than any other pair that happens to agree with itself.
    let receipt_signature = verify::parse_receipt_header(&receipt_header).unwrap_or_else(|e| {
        fatal(&format!(
            "oracle emitted an unparseable receipt header: {e}"
        ))
    });
    checks.oracle(
        "cosmos accepts OUR recomputed digest",
        true,
        ask_oracle_receipt(oracle, &digest, &receipt_signature, &supplier_pubkey),
    );

    // -- negative controls ---------------------------------------------
    println!("\nnegative controls");
    checks.rejects(
        "receipt is refused against another response's payload hash",
        verify::verify_receipt(
            &receipt_header,
            &request_signature,
            &other_payload_hash,
            &supplier_pubkey,
        ),
    );
    checks.rejects(
        "receipt is refused against another request's signature",
        verify::verify_receipt(
            &receipt_header,
            &other_request_signature,
            &want_payload_hash,
            &supplier_pubkey,
        ),
    );
    // ...and cosmos agrees, so the refusal is the contract and not our opinion.
    if let Ok(wrong) = verify::receipt_digest(&request_signature, &other_payload_hash) {
        checks.oracle(
            "cosmos also refuses the wrong-response digest",
            false,
            ask_oracle_receipt(oracle, &wrong, &receipt_signature, &supplier_pubkey),
        );
    }

    // ECDSA is malleable: (R, N-S) verifies wherever (R, S) does, and cosmos
    // rejects the high half. A port that leaves low-S to its crate should see
    // both sides say no here.
    let high_s = to_high_s(&receipt_signature);
    checks.rejects(
        "a high-S receipt is refused",
        verify::verify_receipt(
            &format!("v1.{}", hex::encode(&high_s)),
            &request_signature,
            &want_payload_hash,
            &supplier_pubkey,
        ),
    );
    checks.oracle(
        "cosmos also refuses the high-S receipt",
        false,
        ask_oracle_receipt(oracle, &digest, &high_s, &supplier_pubkey),
    );

    // -- the whole trip ------------------------------------------------
    println!("\nend to end");
    match verify::open_relay_response(
        &relay_response,
        &supplier_pubkey,
        &request_signature,
        Some(&receipt_header),
    ) {
        Ok(http) => {
            checks.equal(
                "payload decodes to the backend's body",
                &http.body,
                &want_body,
            );
            if http.status_code == 200 {
                checks.pass("backend status is 200");
            } else {
                checks.fail(
                    "backend status is 200",
                    &format!("got {}", http.status_code),
                );
            }
            println!("\n  backend said: {}", String::from_utf8_lossy(&http.body));
        }
        Err(e) => checks.fail(
            "open_relay_response accepts the oracle's response",
            &e.to_string(),
        ),
    }

    checks.finish()
}

/// Rewrites a signature as its malleable twin `(R, N-S)`, which is
/// cryptographically just as valid and which cosmos refuses.
fn to_high_s(signature: &[u8]) -> Vec<u8> {
    let s_bytes: [u8; 32] = signature[32..64]
        .try_into()
        .unwrap_or_else(|_| fatal("signature must be 64 bytes"));

    let s = <Scalar as Reduce<U256>>::reduce(&U256::from_be_bytes(s_bytes.into()));
    let flipped = -s;
    if !bool::from(flipped.is_high()) {
        fatal("N - s must be the high half; the oracle's signature was not low-S");
    }

    let mut out = signature[..32].to_vec();
    out.extend_from_slice(&flipped.to_repr());
    out
}

/// Running tally, so one failure does not hide the next.
#[derive(Default)]
struct Checks {
    failed: usize,
}

impl Checks {
    fn pass(&mut self, label: &str) {
        println!("  ok    {label}");
    }

    fn fail(&mut self, label: &str, detail: &str) {
        self.failed += 1;
        println!("  FAIL  {label}: {detail}");
    }

    fn equal(&mut self, label: &str, got: &[u8], want: &[u8]) {
        if got == want {
            self.pass(label);
        } else {
            self.fail(
                label,
                &format!(
                    "\n          got  {}\n          want {}",
                    hex::encode(got),
                    hex::encode(want)
                ),
            );
        }
    }

    fn accepts<T>(&mut self, label: &str, result: Result<T, VerifyError>) {
        match result {
            Ok(_) => self.pass(label),
            Err(e) => self.fail(label, &e.to_string()),
        }
    }

    /// A check that must FAIL. A verifier that has never rejected anything is
    /// indistinguishable from one that returns Ok unconditionally.
    fn rejects<T: std::fmt::Debug>(&mut self, label: &str, result: Result<T, VerifyError>) {
        match result {
            Err(e) => println!("  ok    {label} ({e})"),
            Ok(value) => self.fail(label, &format!("ACCEPTED {value:?}")),
        }
    }

    fn oracle(&mut self, label: &str, want_accepted: bool, verdict: Verdict) {
        if verdict.accepted == want_accepted {
            self.pass(label);
        } else {
            self.fail(label, &verdict.detail);
        }
    }

    fn finish(&self) -> ExitCode {
        if self.failed == 0 {
            println!("\nall checks passed.");
            return ExitCode::SUCCESS;
        }
        println!("\n{} check(s) FAILED.", self.failed);
        ExitCode::FAILURE
    }
}

/// Pulls `"<key>": "<value>"` out of the oracle's JSON.
///
/// Not a JSON parser, and not one worth pulling in: every value read here is
/// hex, or `v1.` followed by hex, in output this repository controls. Two
/// things make the shortcut safe, and both matter.
///
/// The needle includes the OPENING QUOTE. Without it `"request_signature_hex"`
/// also matches inside `"other_request_signature_hex"` -- and the negative
/// control would quietly become a second copy of the positive case, passing
/// while proving nothing.
///
/// And nothing read here contains an escape. `body_text` does (it is JSON
/// inside JSON) and is deliberately not read; `body_bz_hex` carries the same
/// bytes without the quoting problem.
fn json_field(json: &str, key: &str) -> String {
    let needle = format!("\"{key}\": \"");
    let start = json
        .find(&needle)
        .unwrap_or_else(|| fatal(&format!("oracle JSON has no {key}")))
        + needle.len();
    let rest = &json[start..];
    let end = rest
        .find('"')
        .unwrap_or_else(|| fatal(&format!("oracle JSON has an unterminated {key}")));
    rest[..end].to_string()
}

fn json_hex(json: &str, key: &str) -> Vec<u8> {
    hex::decode(json_field(json, key))
        .unwrap_or_else(|e| fatal(&format!("oracle JSON field {key} is not hex: {e}")))
}

fn report_quirk(stats: &SignStats) {
    let rate = stats.short_reductions as f64 / stats.challenges as f64;
    println!("challenges:       {}", stats.challenges);
    println!(
        "short reductions: {} ({:.2}%, predicted ~0.39% = 1/256)",
        stats.short_reductions,
        rate * 100.0,
    );
    println!(
        "                  ^ each one is a challenge a canonical implementation\n\
         \x20                   would have computed differently -- and been rejected for.",
    );
}

struct Verdict {
    accepted: bool,
    detail: String,
}

/// Pipes one signature to `oracle verify`, which hands it to the same ring-go
/// a real relayer runs. Exit code 0 = accepted.
fn ask_oracle(oracle: &Path, m: &[u8; 32], sig: &[u8]) -> Verdict {
    // Both fields are hex, so there is nothing to escape and no JSON library to
    // pull in. Do not copy this shortcut somewhere the values are not hex.
    let request = format!(
        r#"{{"msg_hex":"{}","sig_hex":"{}"}}"#,
        hex::encode(m),
        hex::encode(sig),
    );
    run_oracle(oracle, "verify", Some(&request))
}

/// Pipes a receipt to `oracle verify-receipt`, which hands it to the same
/// cosmos `secp256k1` a real supplier signs with. Exit code 0 = accepted.
///
/// The digest is OURS: the point is not that the oracle can verify its own
/// bytes, it is that the digest this crate computes is the one cosmos accepts.
fn ask_oracle_receipt(oracle: &Path, digest: &[u8; 32], sig: &[u8], pubkey: &[u8]) -> Verdict {
    let request = format!(
        r#"{{"digest_hex":"{}","sig_hex":"{}","pub_hex":"{}"}}"#,
        hex::encode(digest),
        hex::encode(sig),
        hex::encode(pubkey),
    );
    run_oracle(oracle, "verify-receipt", Some(&request))
}

/// Runs one oracle subcommand, optionally writing `request` to its stdin.
fn run_oracle(oracle: &Path, subcommand: &str, request: Option<&str>) -> Verdict {
    let mut child = Command::new(oracle)
        .arg(subcommand)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap_or_else(|e| {
            fatal(&format!(
                "could not run oracle at {}: {e}",
                oracle.display()
            ))
        });

    let mut stdin = child.stdin.take().expect("stdin was piped");
    if let Some(request) = request {
        stdin
            .write_all(request.as_bytes())
            .unwrap_or_else(|e| fatal(&format!("writing to oracle: {e}")));
    }
    drop(stdin); // close the pipe so the oracle's decoder sees EOF

    let out = child
        .wait_with_output()
        .unwrap_or_else(|e| fatal(&format!("waiting for oracle: {e}")));

    Verdict {
        accepted: out.status.success(),
        detail: String::from_utf8_lossy(&out.stdout).trim().to_string(),
    }
}

/// Stands in for the 32-byte SHA-256 signable hash of a relay request. Distinct
/// per message, which is all this needs to be: the challenge preimage also
/// covers L and R, and those are randomised per signature anyway.
fn relay_hash(i: u32) -> [u8; 32] {
    let mut m = [0u8; 32];
    let label = format!("rust-relay-msg-{i:08}");
    m[..label.len()].copy_from_slice(label.as_bytes());
    m
}

fn secret_from_hex(hex_str: &str) -> SecretKey {
    let bytes = hex::decode(hex_str).unwrap_or_else(|e| fatal(&format!("bad key hex: {e}")));
    SecretKey::from_slice(&bytes).unwrap_or_else(|e| fatal(&format!("bad key: {e}")))
}

fn fatal(msg: &str) -> ! {
    eprintln!("fatal: {msg}");
    std::process::exit(2)
}
