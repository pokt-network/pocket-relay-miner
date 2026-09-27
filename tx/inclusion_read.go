package tx

import (
	"context"
	"fmt"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TxInclusion is what the chain says about ONE transaction hash.
//
// It exists because the reconciler's other question -- is this session's claim in
// the module's state -- has a single negative answer that covers several very
// different worlds. This one narrows it, and it narrows it PARTIALLY: read the
// names literally, because the whole point of this type is that the previous
// naming promised more than the data supports.
type TxInclusion int

const (
	// TxInclusionUnknown means the node did not answer in a way that can be
	// interpreted. It is the ZERO VALUE deliberately: anything that forgets to
	// set a verdict says "I could not tell" rather than asserting absence, and
	// absence is the answer that would authorise a resend.
	TxInclusionUnknown TxInclusion = iota

	// TxInclusionNotInBlock means the index does not hold this hash.
	//
	// It does NOT mean the transaction was never sent. CometBFT's own comment on
	// the call underneath says a nil result "could mean the transaction is in
	// the mempool, invalidated, or was not sent in the first place" -- so this
	// verdict still contains three worlds, one of which is a transaction that is
	// alive right now. Resending on it would sign a second transaction while the
	// first is pending, and with per-transaction nonces both can land.
	TxInclusionNotInBlock

	// TxInclusionIncludedOK means the transaction is in a block and its messages
	// succeeded.
	TxInclusionIncludedOK

	// TxInclusionIncludedFailed means the transaction is in a block and its
	// messages failed. This is the verdict the whole read exists to produce: it
	// is the one the module-state query can never distinguish from a delivery
	// failure, and the one that carries a reason.
	TxInclusionIncludedFailed
)

// String is the metric label and the log value. The set is closed.
func (t TxInclusion) String() string {
	switch t {
	case TxInclusionNotInBlock:
		return "not_in_block"
	case TxInclusionIncludedOK:
		return "included_ok"
	case TxInclusionIncludedFailed:
		return "included_failed"
	default:
		return "unknown"
	}
}

// InclusionReadState says whether the post-inclusion read can be used at all.
//
// Three values and not two, and the third has a REACHABLE path rather than only
// a justification: the split is between an answer the node gave and an answer we
// never got. Measured -- an error this RPC's handler returns unclassified
// crosses gRPC as codes.Unknown, while a timeout arrives as DeadlineExceeded and
// an unreachable node as Unavailable. The first means this node's index cannot
// serve the query; the rest mean we could not ask, which says nothing about how
// the node is configured.
//
// Folding the second group into "unavailable" would assert exactly what was not
// measured, and it is an easy thing to do by accident: a switch whose default
// returns unavailable leaves this value in the type, justified in prose, and
// unreachable in practice.
type InclusionReadState string

const (
	InclusionReadAvailable   InclusionReadState = "available"
	InclusionReadUnavailable InclusionReadState = "unavailable"
	InclusionReadUnknown     InclusionReadState = "unknown"
)

// zeroTxHash is a hash no transaction can have, used to ask the node whether it
// can answer at all.
//
// It works because of the ORDER inside the node: the "indexing is disabled"
// check is the first statement of the RPC, before the hash is looked at, so a
// node with tx_index=null answers it without regard to what was asked. A node
// with an index answers "not found". One RPC, no real transaction needed.
const zeroTxHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ReadTxInclusion asks the chain about one hash.
//
// The classification NEVER reads the error's text. That matters more than it
// looks: the SDK maps its own not-found by matching the substring "not found"
// against whatever the node wrote, and a node with its indexer off answers
// "transaction indexing is disabled", which does not contain it -- so today the
// two are distinguishable by accident of wording. Depending on that would put
// the resend decision at the mercy of an upstream error string, which is the
// pattern this delivery layer removed from its own code.
//
// So: only codes.NotFound is absence. Every other error is Unknown, which
// authorises nothing.
func (tc *TxClient) ReadTxInclusion(ctx context.Context, hash string) (TxInclusion, *cosmostypes.TxResponse) {
	if hash == "" {
		return TxInclusionUnknown, nil
	}

	res, err := tc.txClient.GetTx(ctx, &txtypes.GetTxRequest{Hash: hash})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return TxInclusionNotInBlock, nil
		}
		return TxInclusionUnknown, nil
	}
	if res == nil || res.TxResponse == nil {
		return TxInclusionUnknown, nil
	}
	if res.TxResponse.Code == 0 {
		return TxInclusionIncludedOK, res.TxResponse
	}
	return TxInclusionIncludedFailed, res.TxResponse
}

// ReadInclusionForEntry resolves the two hashes an entry can carry -- the
// original submission and, after a resend, the latest one -- into one verdict.
//
// The original send is ONE transaction per batch, so every session in a group
// shares its origHash: this costs one query per group plus one per entry that
// was actually resent, not one per entry.
//
// The precedence is written out because an order like this reads correctly and
// gets implemented backwards without anything complaining:
//
//  1. any hash included and OK wins outright -- the work landed, whichever
//     attempt did it;
//  2. otherwise any hash included and failed, which carries the reason;
//  3. NotInBlock only when BOTH say so, because one hash being absent says
//     nothing about the other;
//  4. anything else is Unknown.
func (tc *TxClient) ReadInclusionForEntry(ctx context.Context, origHash, txHash string) (TxInclusion, *cosmostypes.TxResponse) {
	verdicts := make([]TxInclusion, 0, 2)
	responses := make([]*cosmostypes.TxResponse, 0, 2)

	for _, h := range dedupeHashes(origHash, txHash) {
		v, res := tc.ReadTxInclusion(ctx, h)
		verdicts = append(verdicts, v)
		responses = append(responses, res)
	}
	if len(verdicts) == 0 {
		return TxInclusionUnknown, nil
	}

	for i, v := range verdicts {
		if v == TxInclusionIncludedOK {
			return v, responses[i]
		}
	}
	for i, v := range verdicts {
		if v == TxInclusionIncludedFailed {
			return v, responses[i]
		}
	}
	for _, v := range verdicts {
		if v != TxInclusionNotInBlock {
			return TxInclusionUnknown, nil
		}
	}
	return TxInclusionNotInBlock, nil
}

// dedupeHashes returns the non-empty hashes to ask about, without asking twice
// for the same one: before any resend both fields hold the same value.
func dedupeHashes(origHash, txHash string) []string {
	out := make([]string, 0, 2)
	if origHash != "" {
		out = append(out, origHash)
	}
	if txHash != "" && txHash != origHash {
		out = append(out, txHash)
	}
	return out
}

// ProbeInclusionRead resolves at startup whether this node can answer the
// post-inclusion read, and NEVER prevents the process from starting.
//
// That is the difference from VerifyConn, which refuses to start on a
// misconfigured transaction connection: without that connection nothing can be
// mined, while this read is optional by construction. A node that cannot answer
// costs the operator the CAUSE of a verdict, not the ability to mine.
//
// Resolving it here rather than on the first failed claim is the point: the
// operator learns when the process comes up, instead of during the incident the
// signal exists to explain.
func (tc *TxClient) ProbeInclusionRead(ctx context.Context) InclusionReadState {
	probeCtx, cancel := context.WithTimeout(ctx, txConnProbeTimeout)
	defer cancel()

	_, err := tc.txClient.GetTx(probeCtx, &txtypes.GetTxRequest{Hash: zeroTxHash})
	switch {
	case err == nil:
		// A hash of zeroes resolving to a transaction is not something a real
		// node does; treat it as an answer we cannot interpret rather than
		// evidence the read works.
		return InclusionReadUnknown
	case status.Code(err) == codes.NotFound:
		return InclusionReadAvailable

	case status.Code(err) == codes.Unknown:
		// The node ANSWERED and refused for a reason of its own. Measured: an
		// error the RPC handler returns unclassified -- which for this call is
		// the index being off, or the index failing to read -- crosses gRPC as
		// Unknown, while a deadline arrives as DeadlineExceeded and an
		// unreachable node as Unavailable. Both of the former mean this node's
		// index cannot serve the query, which is what unavailable claims.
		tc.logger.Warn().Err(err).
			Msg("post-inclusion read unavailable: the node refused a transaction lookup")
		return InclusionReadUnavailable

	default:
		// We could not get an answer -- a timeout, a dropped connection. That
		// says nothing about how the node is configured, and calling it
		// unavailable would assert exactly what was not measured. This is the
		// third state the type exists for, and without this branch it was
		// reachable only through a hash of zeroes resolving, which is to say
		// never.
		tc.logger.Warn().Err(err).
			Msg("post-inclusion read state unknown: the node did not answer the probe")
		return InclusionReadUnknown
	}
}

// ErrInclusionReadUnavailable is returned by callers that need to say why they
// have no cause to report.
var ErrInclusionReadUnavailable = fmt.Errorf("post-inclusion read is unavailable on this node")
