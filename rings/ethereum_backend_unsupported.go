//go:build ethereum_secp256k1

package rings

// Building with `-tags ethereum_secp256k1` is refused, on purpose, and this
// file exists only to fail the build when someone tries.
//
// The tag switches go-dleq from its default pure-Go secp256k1 backend
// (curve_decred.go, `!ethereum_secp256k1`) to a cgo one (curve_ethereum.go,
// `cgo && ethereum_secp256k1`). The two backends do not agree.
//
// go-dleq's default HashToScalar finishes with `copy(reduced[:], n.Bytes())`.
// big.Int.Bytes() is the minimal big-endian encoding, so a reduction below
// 2^248 — about 1 in 256 — is LEFT-aligned, i.e. shifted. The cgo backend
// returns the honest, unshifted value. So for ~1 challenge in 256 the two
// compute different scalars, and a relay signed under one backend is rejected
// by a verifier running the other.
//
// That makes the tag a silent consensus fork. A binary built with it would
// mine relays that the chain, the gateways, and every other relayer reject at a rate
// low enough to be mistaken for a flaky network. It was originally tried as a
// way to buy speed and did not deliver any — only latency and problems — so
// nothing is being given up by refusing it.
//
// The signature scheme is defined by the default backend's behaviour, quirk
// included. If go-dleq ever makes the two backends byte-identical, this file
// can go — and that change must be coordinated fleet-wide, because it moves
// signature bytes.
//
// See examples/relay-signing/README.md for the measured divergence.

type buildTagEthereumSecp256k1IsNotSupported struct{}

var _ buildTagEthereumSecp256k1IsNotSupported = "the ethereum_secp256k1 build tag is not supported: it selects a go-dleq backend whose HashToScalar disagrees with the default for ~1 in 256 challenges, which silently forks ring signature validity. Build without it. See rings/ethereum_backend_unsupported.go"
