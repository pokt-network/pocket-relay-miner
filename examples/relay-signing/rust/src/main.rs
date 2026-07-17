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

use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode, Stdio};
use k256::elliptic_curve::Generate;
use k256::{PublicKey, SecretKey};
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
            "usage: {} <path-to-oracle> [count] [--negative-control]",
            args[0]
        );
        return ExitCode::from(2);
    };
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
    let mut child = Command::new(oracle)
        .arg("verify")
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

    // Both fields are hex, so there is nothing to escape and no JSON library to
    // pull in. Do not copy this shortcut somewhere the values are not hex.
    let request = format!(
        r#"{{"msg_hex":"{}","sig_hex":"{}"}}"#,
        hex::encode(m),
        hex::encode(sig),
    );

    let mut stdin = child.stdin.take().expect("stdin was piped");
    stdin
        .write_all(request.as_bytes())
        .unwrap_or_else(|e| fatal(&format!("writing to oracle: {e}")));
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
