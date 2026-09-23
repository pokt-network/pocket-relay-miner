//go:build test

package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// Shapes of SyntheticRelayBytes. There are no real payloads to test against and
// the traffic is as diverse as the chains and APIs served, so the corpus spans
// what compresses and what does not: JSON-RPC with hex (EVM), JSON with base64
// (Cosmos), binary (gRPC), and natural text (an LLM prompt).
const (
	RelayShapeEVMJSON    = "evm_json"
	RelayShapeCosmosJSON = "cosmos_json"
	RelayShapeBinary     = "binary"
	RelayShapeText       = "text"
)

// RelayShapes lists every shape SyntheticRelayBytes builds.
var RelayShapes = []string{RelayShapeEVMJSON, RelayShapeCosmosJSON, RelayShapeBinary, RelayShapeText}

// relayEnvelopeBytes is the size of the incompressible head every synthetic
// relay starts with, standing in for the request's session header and ring
// signature.
const relayEnvelopeBytes = 1100

// ChainedHashBytes fills n bytes with chained SHA-256: deterministic, and as
// incompressible as the hashes and signatures a real relay carries. Filler such
// as bytes.Repeat or make([]byte, n) compresses to almost nothing and lets a test
// about sizes pass for the wrong reason once relays are compressed.
func ChainedHashBytes(seed string, n int) []byte {
	out := make([]byte, 0, n+sha256.Size)
	prev := sha256.Sum256([]byte(seed))
	for len(out) < n {
		out = append(out, prev[:]...)
		prev = sha256.Sum256(prev[:])
	}
	return out[:n]
}

var textWords = strings.Fields(`the relay miner serves requests for applications and signs every
response so the supplier can claim the work on chain summarize this document explain how
sessions end and why a proof is required describe the difference between a claim and a
proof in plain words with an example of a batch request and its response`)

// SyntheticRelayBytes returns exactly n deterministic bytes of the given shape,
// starting with an incompressible envelope when n allows one. An unknown shape
// builds binary.
func SyntheticRelayBytes(shape string, n int) []byte {
	var b strings.Builder
	b.Write(ChainedHashBytes("envelope/"+shape, min(relayEnvelopeBytes, n)))
	// Word choices for the text shape: hashed, so the text never falls into a
	// cycle a compressor would reduce to nothing.
	picks := ChainedHashBytes("words/"+shape, n/2+1)
	for i := 0; b.Len() < n; i++ {
		switch shape {
		case RelayShapeEVMJSON:
			fmt.Fprintf(&b, `{"jsonrpc":"2.0","id":%d,"method":"eth_call","params":[{"to":"0x%s","data":"0x%s"},"latest"]},`,
				i, hex.EncodeToString(ChainedHashBytes(fmt.Sprint("to", i%16), 20)), hex.EncodeToString(ChainedHashBytes(fmt.Sprint("data", i), 68)))
		case RelayShapeCosmosJSON:
			fmt.Fprintf(&b, `{"jsonrpc":"2.0","id":%d,"method":"broadcast_tx_sync","params":{"tx":"%s"}},`,
				i, base64.StdEncoding.EncodeToString(ChainedHashBytes(fmt.Sprint("tx", i), 240)))
		case RelayShapeText:
			b.WriteString(textWords[int(picks[i%len(picks)])%len(textWords)])
			b.WriteByte(' ')
		default:
			b.Write(ChainedHashBytes(fmt.Sprint("bin", i), 4096))
		}
	}
	return []byte(b.String()[:n])
}
