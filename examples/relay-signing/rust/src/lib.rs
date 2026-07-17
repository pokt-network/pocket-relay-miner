//! A from-scratch bLSAG ring signer for Pocket Network relays.
//!
//! A Pocket relay is authorised by a ring signature over `[appPubKey,
//! gatewayPubKey]`, signed by whichever of the two is sending. The relayer
//! verifies it with [ring-go], so "correct" here means exactly one thing:
//! **ring-go accepts the bytes.** Round-tripping your own sign/verify proves
//! nothing -- an implementation can be perfectly self-consistent and still be
//! rejected by every relayer on the network.
//!
//! This is a translation of the Go reference in `../oracle`, not an
//! independent design. Where it looks odd, it is because the Go it must
//! interoperate with is odd, and the comments say so.
//!
//! # The two traps
//!
//! 1. **SHA3-512 means FIPS-202 SHA-3, not Keccak.** Ethereum-adjacent code
//!    says "sha3" and means Keccak-256 (the pre-standardisation padding). This
//!    scheme means the standardised one. The `sha3` crate ships both; picking
//!    `Keccak512` here produces a signer that is wrong 100% of the time --
//!    which, mercifully, you notice immediately.
//!
//! 2. **[`hash_to_scalar`] left-aligns short reductions.** This one is wrong
//!    only ~1 time in 256, which you do *not* notice immediately. It is the
//!    reason this crate exists in this form. See [`hash_to_scalar`].
//!
//! # Testing
//!
//! Trap 2 fires on roughly 1 signature in 128 for a 2-member ring. A ten-
//! signature test therefore passes for a *wrong* implementation about 93% of
//! the time. Do not trust a small sample: `src/main.rs` signs 1500+ messages
//! and requires the Go oracle to accept every single one.
//!
//! [ring-go]: https://github.com/pokt-network/ring-go

use k256::{
    AffinePoint, FieldBytes, ProjectivePoint, PublicKey, Scalar, SecretKey, U256,
    elliptic_curve::{
        Field,
        bigint::{Encoding, NonZero, U512},
        ops::Reduce,
        point::DecompressPoint,
        sec1::ToSec1Point,
        subtle::Choice,
    },
};

use getrandom::{SysRng, rand_core::UnwrapErr};
use sha3::{Digest, Sha3_256, Sha3_512};
use thiserror::Error;

/// The secp256k1 group order N, widened to 512 bits so a SHA3-512 digest can be
/// reduced by it directly (mirroring Go's `big.Int.Mod`).
const CURVE_ORDER_WIDE: U512 = U512::from_be_hex(
    "0000000000000000000000000000000000000000000000000000000000000000\
     fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141",
);

/// Compressed SEC1 points are 33 bytes: a parity byte plus the x coordinate.
const COMPRESSED_POINT_LEN: usize = 33;

/// Scalars are 32 bytes on the wire.
const SCALAR_LEN: usize = 32;

/// Why a signature could not be produced.
///
/// Every variant except the index checks is cryptographically unreachable --
/// they exist so this crate never panics on a caller's behalf, not because you
/// should expect to see them.
#[derive(Error, Debug, Clone, PartialEq, Eq)]
pub enum SignError {
    /// The ring had no members.
    #[error("ring is empty")]
    EmptyRing,
    /// The ring size does not fit the 4-byte length prefix of the wire format.
    #[error("ring size {0} exceeds u32")]
    RingTooLarge(usize),
    /// `our_idx` does not point at a ring member.
    #[error("signer index {our_idx} out of range for ring of {size}")]
    SignerIndexOutOfRange { our_idx: usize, size: usize },
    /// [`hash_to_curve`] failed to find a point in 128 attempts (p ~ 2^-128).
    #[error("hash_to_curve found no point in 128 attempts")]
    HashToCurve,
    /// A point came out as the identity, which has no 33-byte encoding.
    /// Requires a random scalar to land on exactly zero (p ~ 2^-256).
    #[error("point is the identity and has no compressed encoding")]
    IdentityPoint,
}

// ---------------------------------------------------------------------------
// Primitive 1: hash_to_scalar  (the quirk lives here)
// ---------------------------------------------------------------------------

/// The intermediate state of [`hash_to_scalar`], exposed so a test can prove
/// the quirk actually fired rather than assume it.
///
/// You do not need this to sign. You need it to *trust* your signer: the
/// left-align below is invisible in a passing test unless you count it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Reduction {
    /// Length of the minimal big-endian encoding of `n = SHA3-512(input) mod N`
    /// -- i.e. what Go's `n.Bytes()` would have returned. Normally 32. When it
    /// is less, the quirk fires.
    pub minimal_len: usize,
    /// The 32 bytes actually used as the scalar. Equal to the canonical
    /// encoding of `n` when `minimal_len == 32`, and to `n << 8*(32-minimal_len)`
    /// otherwise.
    pub encoded: [u8; SCALAR_LEN],
}

impl Reduction {
    /// Whether the left-align quirk fired for this input (~1 in 256).
    pub fn is_short(&self) -> bool {
        self.minimal_len < SCALAR_LEN
    }

    /// The resulting scalar.
    pub fn scalar(&self) -> Scalar {
        // `reduce_bytes`, NOT `Scalar::from_repr`. from_repr is a CtOption that
        // rejects anything >= N, and `encoded` is not guaranteed to be a
        // canonical reduction -- shifting left can, in principle, push it over
        // N. (It needs n >= N/256, so p ~ 2^-127; you will not see it. But
        // `reduce` is both correct and free, and matches what Go does, which is
        // to apply `mod N` lazily at every point where the scalar gets used.)
        <Scalar as Reduce<U256>>::reduce(&U256::from_be_bytes(self.encoded.into()))
    }
}

/// Hashes arbitrary bytes to a secp256k1 scalar, **bug-for-bug compatible with
/// go-dleq's `HashToScalar`** (secp256k1/curve_decred.go).
///
/// The algorithm is `int(SHA3-512(input)) mod N`, and then the interesting
/// part. Go writes the result into a fixed array like this:
///
/// ```go
/// var reduced [32]byte
/// copy(reduced[:], n.Bytes())   // NOT n.FillBytes(reduced[:])
/// ```
///
/// `big.Int.Bytes()` returns the **minimal** big-endian encoding -- no leading
/// zeroes -- and `copy` writes from offset 0. So when the reduction happens to
/// need fewer than 32 bytes, the value lands **LEFT-aligned**, with the slack
/// as trailing zeroes. That is not padding: it multiplies the scalar by 256 for
/// every byte it was short. `n` needs fewer than 32 bytes whenever it lands
/// below 2^248, which is about 1 input in 256.
///
/// A port that right-aligns -- the natural choice, and what `FillBytes`,
/// `to_bytes`, and every sane API does -- computes a different scalar for those
/// inputs. The challenge chain then fails to close and the relayer rejects the
/// relay. With the 2-member `[app, gateway]` ring that is ~1 relay in 128:
/// often enough to cost you money, rare enough to look like a flaky network.
///
/// Note the contrast with [`encode_scalar`], which *is* right-aligned. Both
/// encodings are 32-byte big-endian scalars; only this one is quirked. Getting
/// them backwards fails in the opposite direction, just as intermittently.
pub fn hash_to_scalar(input: &[u8]) -> Scalar {
    hash_to_scalar_reduction(input).scalar()
}

/// [`hash_to_scalar`], with the intermediate state kept. See [`Reduction`].
pub fn hash_to_scalar_reduction(input: &[u8]) -> Reduction {
    // SHA3-512 = FIPS-202. Not Keccak512. Not SHA-512.
    let digest = Sha3_512::digest(input);

    // n = int(digest) mod N, exactly as Go's big.Int does it: the digest is a
    // 512-bit big-endian integer, reduced by the 256-bit group order.
    let modulus = NonZero::new(CURVE_ORDER_WIDE).unwrap();
    let n = U512::from_be_slice(&digest) % modulus;

    // n < N < 2^256, so its canonical form is the low 32 bytes.
    let wide = n.to_be_bytes();
    let canonical: [u8; SCALAR_LEN] = wide[SCALAR_LEN..].try_into().expect("64 - 32 == 32");

    // Now the quirk. Go's `n.Bytes()` is `canonical` with leading zeroes
    // stripped; `copy(reduced[:], ...)` puts that at offset 0 and leaves the
    // tail zeroed. Stripping k leading zeroes and re-appending k trailing ones
    // is precisely a left-shift by k bytes.
    let leading_zeros = canonical.iter().take_while(|&&b| b == 0).count();
    let mut encoded = [0u8; SCALAR_LEN];
    encoded[..SCALAR_LEN - leading_zeros].copy_from_slice(&canonical[leading_zeros..]);

    Reduction {
        minimal_len: SCALAR_LEN - leading_zeros,
        encoded,
    }
}

// ---------------------------------------------------------------------------
// Primitive 2: hash_to_curve
// ---------------------------------------------------------------------------

/// Maps a compressed public key to a curve point by try-and-increment,
/// mirroring ring-go's `hashToCurveSecp256k1` (helpers.go).
///
/// Take SHA3-256 of the key, read the digest as a field element `x`, and return
/// the point on `y^2 = x^3 + 7` with **even** `y`. Only about half of all `x`
/// values are on the curve; on a miss, re-hash **the digest** -- not the
/// original key -- and try again.
///
/// The input is the 33-byte compressed encoding, because that is what gets
/// hashed. Passing an uncompressed key, or a `PublicKey` you compress
/// differently, silently produces a different point.
pub fn hash_to_curve(compressed_pubkey: &[u8; COMPRESSED_POINT_LEN]) -> Option<AffinePoint> {
    /// Go bounds the loop at 128 to guarantee termination. Each attempt
    /// succeeds with p ~ 1/2, so reaching the bound is a 2^-128 event.
    const MAX_ATTEMPTS: usize = 128;

    let mut digest: [u8; 32] = Sha3_256::digest(compressed_pubkey).into();

    for _ in 0..MAX_ATTEMPTS {
        // `Choice::from(0)` = "y is odd? no" = take the even root. This is
        // ring-go's `DecompressY(fe, false, ...)`.
        //
        // (Pedantic divergence, listed for honesty: decred's FieldVal::SetBytes
        // returns an ignored overflow flag and keeps computing, so Go would use
        // `digest mod p` for a digest >= p, where k256 rejects it and re-hashes.
        // p is within 2^32 of 2^256, so this needs a digest in a ~2^-224 slice
        // of the output space. It has never happened and never will.)
        let candidate = AffinePoint::decompress(&FieldBytes::from(digest), Choice::from(0));
        if let Some(point) = Option::<AffinePoint>::from(candidate) {
            return Some(point);
        }
        digest = Sha3_256::digest(digest).into();
    }

    None
}

// ---------------------------------------------------------------------------
// Primitive 3: the wire encoding for scalars
// ---------------------------------------------------------------------------

/// Encodes a scalar for the wire: 32 bytes big-endian, zero-padded on the
/// **LEFT**. Canonical, boring, exactly what you would guess.
///
/// It is spelled out as its own function only to sit next to
/// [`hash_to_scalar`] and make the contrast impossible to miss: scalars *on the
/// wire* are right-aligned; the scalar *derived inside* `hash_to_scalar` is
/// left-aligned. Same curve, same 32 bytes, opposite conventions.
pub fn encode_scalar(scalar: &Scalar) -> [u8; SCALAR_LEN] {
    scalar.to_bytes().into()
}

// ---------------------------------------------------------------------------
// The signer
// ---------------------------------------------------------------------------

/// What happened inside one signature.
///
/// Only interesting to tests: the left-align quirk is unobservable from the
/// outside, so "my 1500 signatures passed" is a much weaker claim than "my 1500
/// signatures passed *and* the quirk fired 12 times while they did".
#[derive(Debug, Default, Clone, Copy, PartialEq, Eq)]
pub struct SignStats {
    /// Challenges computed. Equal to the ring size.
    pub challenges: usize,
    /// How many of those hit [`hash_to_scalar`]'s left-align quirk.
    pub short_reductions: usize,
}

impl SignStats {
    /// Accumulates another signature's stats into this one.
    pub fn add(&mut self, other: SignStats) {
        self.challenges += other.challenges;
        self.short_reductions += other.short_reductions;
    }
}

/// The scalar derivation used for challenges. Parameterised only so the tests
/// can substitute the canonical (wrong) derivation and prove the oracle
/// rejects it -- mirroring the Go reference's `hashToScalarFn(canonical bool)`.
type HashToScalarFn = fn(&[u8]) -> Reduction;

/// Signs a 32-byte relay hash with a bLSAG ring signature.
///
/// For a Pocket relay the ring is `[appPubKey, gatewayPubKey]` **in that order,
/// unsorted**, and the gateway signs, so `our_idx` is 1. `m` is the 32-byte
/// SHA-256 signable hash of the relay request.
///
/// Returns the 199-byte (`69 + 65n`) wire signature the relayer expects.
///
/// ```no_run
/// # use pocket_relay_signing::build_relay_signature;
/// # fn demo(app: k256::PublicKey, gw: k256::PublicKey, gw_key: k256::SecretKey, m: [u8; 32])
/// # -> Result<(), Box<dyn std::error::Error>> {
/// let sig = build_relay_signature(&m, &[app, gw], &gw_key, 1)?;
/// assert_eq!(sig.len(), 199);
/// # Ok(()) }
/// ```
pub fn build_relay_signature(
    m: &[u8; 32],
    ring_pubkeys: &[PublicKey],
    priv_key: &SecretKey,
    our_idx: usize,
) -> Result<Vec<u8>, SignError> {
    build_relay_signature_with_stats(m, ring_pubkeys, priv_key, our_idx).map(|(sig, _)| sig)
}

/// [`build_relay_signature`], reporting what happened inside. See [`SignStats`].
pub fn build_relay_signature_with_stats(
    m: &[u8; 32],
    ring_pubkeys: &[PublicKey],
    priv_key: &SecretKey,
    our_idx: usize,
) -> Result<(Vec<u8>, SignStats), SignError> {
    sign_inner(m, ring_pubkeys, priv_key, our_idx, hash_to_scalar_reduction)
}

fn sign_inner(
    m: &[u8; 32],
    ring_pubkeys: &[PublicKey],
    priv_key: &SecretKey,
    our_idx: usize,
    hash: HashToScalarFn,
) -> Result<(Vec<u8>, SignStats), SignError> {
    let size = ring_pubkeys.len();
    if size == 0 {
        return Err(SignError::EmptyRing);
    }
    if our_idx >= size {
        return Err(SignError::SignerIndexOutOfRange { our_idx, size });
    }
    let size_prefix = u32::try_from(size).map_err(|_| SignError::RingTooLarge(size))?;

    let x = *priv_key.to_nonzero_scalar().as_ref();
    let mut stats = SignStats::default();

    // Needed twice each: to hash to the curve, and to serialise.
    let compressed: Vec<[u8; COMPRESSED_POINT_LEN]> = ring_pubkeys
        .iter()
        .map(|pk| compress(&pk.to_projective()))
        .collect::<Result<_, _>>()?;

    // The key image I = x*H(P_j) is what makes bLSAG *linkable*: it is fixed by
    // our key alone, so two signatures from the same key share it, while
    // revealing nothing about which ring member produced them.
    let h_our =
        ProjectivePoint::from(hash_to_curve(&compressed[our_idx]).ok_or(SignError::HashToCurve)?);
    let image = h_our * x;

    let mut c = vec![Scalar::ZERO; size];
    let mut s = vec![Scalar::ZERO; size];

    // Seed the chain at our own position with a random u, committing to
    // (u*G, u*H_j) without yet knowing c[j].
    let u = Scalar::random(&mut UnwrapErr(SysRng));
    c[(our_idx + 1) % size] = challenge(
        m,
        &(ProjectivePoint::GENERATOR * u),
        &(h_our * u),
        hash,
        &mut stats,
    )?;

    // Walk the rest of the ring forward. Each decoy gets a random s[idx], and
    // its L/R are *derived* from the challenge we already have -- so they
    // verify without us knowing that member's key.
    for i in 1..size {
        let idx = (our_idx + i) % size;
        s[idx] = Scalar::random(&mut UnwrapErr(SysRng));

        let p_idx = ring_pubkeys[idx].to_projective();
        let h_idx =
            ProjectivePoint::from(hash_to_curve(&compressed[idx]).ok_or(SignError::HashToCurve)?);

        let l = p_idx * c[idx] + ProjectivePoint::GENERATOR * s[idx];
        let r = image * c[idx] + h_idx * s[idx];
        c[(idx + 1) % size] = challenge(m, &l, &r, hash, &mut stats)?;
    }

    // Close the ring. Having gone all the way round we now know c[j], and only
    // the real key can produce the s[j] that makes L_j/R_j reproduce the
    // commitment we seeded with: s_j = u - c_j*x, so s_j*G + c_j*P_j = u*G.
    s[our_idx] = u - c[our_idx] * x;

    // Wire format: [4B BE n][32B c[0]][33B I][ n x (32B s_i || 33B P_i) ].
    let mut out = Vec::with_capacity(69 + 65 * size);
    out.extend_from_slice(&size_prefix.to_be_bytes());
    // c[0], the ring's *first* challenge -- not c[our_idx]. The verifier
    // re-walks the chain from index 0 and checks it lands back on this value,
    // which is also why the signature does not leak our_idx.
    out.extend_from_slice(&encode_scalar(&c[0]));
    out.extend_from_slice(&compress(&image)?);
    for (s_i, pub_i) in s.iter().zip(&compressed) {
        out.extend_from_slice(&encode_scalar(s_i));
        out.extend_from_slice(pub_i);
    }

    Ok((out, stats))
}

/// `challenge(m, L, R) = hash_to_scalar(m || L || R)` over compressed points.
fn challenge(
    m: &[u8; 32],
    l: &ProjectivePoint,
    r: &ProjectivePoint,
    hash: HashToScalarFn,
    stats: &mut SignStats,
) -> Result<Scalar, SignError> {
    let mut preimage = Vec::with_capacity(32 + 2 * COMPRESSED_POINT_LEN);
    preimage.extend_from_slice(m);
    preimage.extend_from_slice(&compress(l)?);
    preimage.extend_from_slice(&compress(r)?);

    let reduction = hash(&preimage);
    stats.challenges += 1;
    if reduction.is_short() {
        stats.short_reductions += 1;
    }
    Ok(reduction.scalar())
}

/// 33-byte compressed SEC1 encoding.
fn compress(point: &ProjectivePoint) -> Result<[u8; COMPRESSED_POINT_LEN], SignError> {
    let encoded = point.to_affine().to_sec1_point(true);
    // The identity encodes to a single 0x00 byte, so this is not a formality --
    // it is the only thing standing between a 2^-256 event and a panic.
    encoded
        .as_bytes()
        .try_into()
        .map_err(|_| SignError::IdentityPoint)
}

// ---------------------------------------------------------------------------
// The negative control
//
// A test suite that has never failed is not evidence. These two exist so the
// harness can sign with the *wrong* primitive and show the Go oracle rejecting
// the result at the predicted ~0.78% -- same code path, one function swapped.
// Without that, "1500 signatures passed" could just mean the oracle says yes to
// everything. They mirror the Go reference's `hashToScalarFn(canonical bool)`.
// ---------------------------------------------------------------------------

/// The canonical (right-aligning) derivation -- i.e. the bug.
///
/// Reduce mod N and encode the result the way every sane API encodes it, which
/// for ~255 of 256 inputs is byte-identical to [`hash_to_scalar`] and for the
/// remaining 1 is off by a factor of 256. Do not call this to sign.
#[doc(hidden)]
pub fn canonical_reduction_impl(input: &[u8]) -> Reduction {
    let digest = Sha3_512::digest(input);
    let modulus = NonZero::new(CURVE_ORDER_WIDE).unwrap();
    let n = U512::from_be_slice(&digest) % modulus;

    let wide = n.to_be_bytes();
    let encoded: [u8; SCALAR_LEN] = wide[SCALAR_LEN..].try_into().expect("64 - 32 == 32");

    Reduction {
        minimal_len: SCALAR_LEN - encoded.iter().take_while(|&&b| b == 0).count(),
        encoded, // right-aligned: zero-padded at the FRONT. The natural choice.
    }
}

/// Signs with the canonical (wrong) derivation. See [`canonical_reduction_impl`].
#[doc(hidden)]
pub fn build_relay_signature_canonical(
    m: &[u8; 32],
    ring_pubkeys: &[PublicKey],
    priv_key: &SecretKey,
    our_idx: usize,
) -> Result<(Vec<u8>, SignStats), SignError> {
    sign_inner(m, ring_pubkeys, priv_key, our_idx, canonical_reduction_impl)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use k256::elliptic_curve::Generate;

    // Pinned against `oracle vectors`. These are the deterministic pieces
    // underneath a (randomised, undiffable) signature: if the harness fails,
    // bisect here first.
    const APP_PUB_HEX: &str = "0397896e9b106df70124a856861cc9be52fac9980e2c7a118a36c19d0198692cc5";
    const GW_PUB_HEX: &str = "02bbbf99abdcddac27350bca272d7146187c091aacfc1c6f90819c9b6daf4fe846";

    fn hex32(s: &str) -> [u8; 32] {
        hex::decode(s).unwrap().try_into().unwrap()
    }

    fn hex33(s: &str) -> [u8; 33] {
        hex::decode(s).unwrap().try_into().unwrap()
    }

    /// `oracle vectors` -> hash_to_scalar[0]: an ordinary, full-width reduction.
    #[test]
    fn hash_to_scalar_matches_oracle_vector_ordinary() {
        let got = hash_to_scalar_reduction(b"pocket");
        assert_eq!(
            got.minimal_len, 32,
            "this vector must NOT be a short reduction"
        );
        assert!(!got.is_short());
        assert_eq!(
            hex::encode(encode_scalar(&got.scalar())),
            "43913f269ab77ad581e23102878c906eacd55fd9e973714f0ebbe9b55ad56fef"
        );
    }

    /// `oracle vectors` -> hash_to_scalar[1]: the SHORT reduction. This vector
    /// exists specifically to catch a canonical implementation, and is the one
    /// that matters. Note the trailing 0x00 in the expected output -- that is
    /// the left-shift, visible in the open.
    #[test]
    fn hash_to_scalar_matches_oracle_vector_short_reduction() {
        let input = hex::decode("72656c61792d6d73672d333439").unwrap(); // "relay-msg-349"
        let got = hash_to_scalar_reduction(&input);

        assert_eq!(
            got.minimal_len, 31,
            "the oracle pins this input at a 31-byte n"
        );
        assert!(got.is_short());
        assert_eq!(
            hex::encode(encode_scalar(&got.scalar())),
            "bd133cd1452701ae1d3910e3ad3fb1a3dddd7fc62cce0521503d6f6e658dd600",
        );
    }

    /// The negative control for trap 2, made deterministic: pin what a
    /// right-aligning implementation computes for the short vector, and assert
    /// the real derivation does not agree with it. If someone ever "fixes"
    /// `hash_to_scalar` to be sane, this fails loudly instead of silently
    /// costing 1-in-128 relays.
    #[test]
    fn canonical_derivation_diverges_on_short_reductions() {
        let input = hex::decode("72656c61792d6d73672d333439").unwrap();

        let quirked = hash_to_scalar_reduction(&input);
        let canonical = canonical_reduction_impl(&input);

        // Same value, shifted one byte: the canonical form has a LEADING zero
        // where the quirked form has a TRAILING one.
        assert_eq!(
            hex::encode(canonical.encoded),
            "00bd133cd1452701ae1d3910e3ad3fb1a3dddd7fc62cce0521503d6f6e658dd6",
        );
        assert_eq!(
            hex::encode(quirked.encoded),
            "bd133cd1452701ae1d3910e3ad3fb1a3dddd7fc62cce0521503d6f6e658dd600",
        );
        assert_ne!(quirked.scalar(), canonical.scalar());
    }

    /// ...and on the ~255/256 of inputs that reduce to a full 32 bytes, the two
    /// agree exactly. That is what makes the bug survive a small test suite.
    #[test]
    fn canonical_derivation_agrees_on_full_width_reductions() {
        let mut compared = 0;
        for i in 0..512u32 {
            let input = format!("agreement-probe-{i}");
            let quirked = hash_to_scalar_reduction(input.as_bytes());
            if quirked.is_short() {
                continue;
            }
            assert_eq!(
                quirked.scalar(),
                canonical_reduction_impl(input.as_bytes()).scalar(),
                "full-width reduction of {input:?} must be unaffected by the quirk",
            );
            compared += 1;
        }
        assert!(
            compared > 400,
            "expected most probes to be full-width, got {compared}"
        );
    }

    /// The quirk must fire at ~1/256. A rate far off that means the reduction
    /// itself is wrong, not just its encoding. Bounds are deliberately loose:
    /// this is a probabilistic property and must not flake.
    #[test]
    fn short_reductions_occur_at_the_predicted_rate() {
        const PROBES: u32 = 20_000;
        let short = (0..PROBES)
            .filter(|i| hash_to_scalar_reduction(format!("rate-probe-{i}").as_bytes()).is_short())
            .count();

        let rate = short as f64 / f64::from(PROBES);
        assert!(
            (0.002..0.007).contains(&rate),
            "short-reduction rate {:.4}% over {PROBES} probes, expected ~0.39% (1/256)",
            rate * 100.0,
        );
    }

    /// `oracle vectors` -> hash_to_curve, both ring members.
    #[test]
    fn hash_to_curve_matches_oracle_vectors() {
        for (input, want) in [
            (
                APP_PUB_HEX,
                "02f72a1f27fa696323f979f608a525372567ad7c532736465db78c7d7118ce46e0",
            ),
            (
                GW_PUB_HEX,
                "02703716bd1d384b39442d0fe0a121ef050bd663d6a2a297902db45fd5874e0ed2",
            ),
        ] {
            let point = hash_to_curve(&hex33(input)).expect("hash_to_curve must find a point");
            let got = compress(&ProjectivePoint::from(point)).unwrap();
            assert_eq!(hex::encode(got), want, "hash_to_curve({input})");
        }
    }

    /// Every output must have even Y, i.e. a 0x02 parity prefix. The odd root
    /// is just as much "a point with that X" -- picking it silently gives a
    /// different H, and every signature is rejected.
    #[test]
    fn hash_to_curve_always_returns_even_y() {
        for i in 0..64u8 {
            let mut key = hex33(APP_PUB_HEX);
            key[32] = i; // vary the low byte of X; still just 33 bytes to hash
            let point = hash_to_curve(&key).expect("must find a point");
            let encoded = compress(&ProjectivePoint::from(point)).unwrap();
            assert_eq!(encoded[0], 0x02, "prefix must mark an even Y, probe {i}");
        }
    }

    /// `oracle vectors` -> scalar_encoding: the right-aligned contrast.
    #[test]
    fn encode_scalar_is_left_padded() {
        assert_eq!(
            hex::encode(encode_scalar(&Scalar::ONE)),
            "0000000000000000000000000000000000000000000000000000000000000001",
        );
    }

    #[test]
    fn signature_has_the_expected_wire_layout() {
        let (app, gw, gw_key) = test_ring();
        let sig = build_relay_signature(&[7u8; 32], &[app, gw], &gw_key, 1).unwrap();

        assert_eq!(sig.len(), 199, "69 + 65*2");
        assert_eq!(&sig[0..4], &[0, 0, 0, 2], "ring size prefix, big-endian");

        // The ring is embedded verbatim, in order, unsorted.
        assert_eq!(hex::encode(&sig[101..134]), APP_PUB_HEX);
        assert_eq!(hex::encode(&sig[166..199]), GW_PUB_HEX);

        // The key image is a compressed point.
        assert!(matches!(sig[36], 0x02 | 0x03), "key image parity byte");
    }

    /// The key image is `x*H(P_j)`: fixed by our key, so it repeats across
    /// signatures even though every other byte is randomised. (This is what
    /// makes bLSAG linkable, and how a relayer spots a replayed signer.)
    #[test]
    fn key_image_is_stable_across_signatures() {
        let (app, gw, gw_key) = test_ring();
        let image_of = |m: [u8; 32]| {
            build_relay_signature(&m, &[app, gw], &gw_key, 1).unwrap()[36..69].to_vec()
        };

        assert_eq!(image_of([1u8; 32]), image_of([2u8; 32]));
    }

    /// Stats must count one challenge per ring member, whatever the ring size.
    #[test]
    fn stats_count_one_challenge_per_ring_member() {
        let (app, gw, gw_key) = test_ring();
        let (_, stats) =
            build_relay_signature_with_stats(&[9u8; 32], &[app, gw], &gw_key, 1).unwrap();
        assert_eq!(stats.challenges, 2);
        assert!(stats.short_reductions <= stats.challenges);
    }

    #[test]
    fn rejects_a_signer_index_outside_the_ring() {
        let (app, gw, gw_key) = test_ring();
        assert_eq!(
            build_relay_signature(&[0u8; 32], &[app, gw], &gw_key, 2),
            Err(SignError::SignerIndexOutOfRange {
                our_idx: 2,
                size: 2
            }),
        );
    }

    #[test]
    fn rejects_an_empty_ring() {
        let (_, _, gw_key) = test_ring();
        assert_eq!(
            build_relay_signature(&[0u8; 32], &[], &gw_key, 0),
            Err(SignError::EmptyRing),
        );
    }

    /// Larger rings must produce `69 + 65n` bytes from any signing position --
    /// the `(our_idx + i) % size` walk is easy to get subtly wrong and a
    /// 2-member ring barely exercises it.
    #[test]
    fn supports_larger_rings_from_any_position() {
        let keys: Vec<SecretKey> = (0..5).map(|_| SecretKey::generate()).collect();
        let ring: Vec<PublicKey> = keys.iter().map(SecretKey::public_key).collect();

        for (our_idx, key) in keys.iter().enumerate() {
            let sig = build_relay_signature(&[3u8; 32], &ring, key, our_idx).unwrap();
            assert_eq!(sig.len(), 69 + 65 * 5, "signing from index {our_idx}");
            assert_eq!(&sig[0..4], &[0, 0, 0, 5]);
        }
    }

    // -- helpers ---------------------------------------------------------

    /// The `[app, gateway]` ring from `oracle vectors`, and the gateway key.
    fn test_ring() -> (PublicKey, PublicKey, SecretKey) {
        let app = SecretKey::from_slice(&hex32(
            "2d00ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ec0a",
        ))
        .unwrap();
        let gw = SecretKey::from_slice(&hex32(
            "1a11ef074d9b51e46886dc9a1df11e7b986611d0f336bdcf1f0adce3e037ab11",
        ))
        .unwrap();

        // Pin the derived pubkeys against the oracle: if these drift, every
        // other test in this file is testing the wrong ring.
        assert_eq!(hex::encode(app.public_key().to_sec1_bytes()), APP_PUB_HEX);
        assert_eq!(hex::encode(gw.public_key().to_sec1_bytes()), GW_PUB_HEX);

        (app.public_key(), gw.public_key(), gw)
    }
}
