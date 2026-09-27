package tx

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/cometbft/cometbft/crypto/tmhash"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TxStage names where in the submission a transaction was rejected. The three
// stages produce structurally different rejections, so most properties of a
// TxRejection have to be read together with the stage that produced it.
type TxStage string

const (
	// TxStageSimulate is the gas simulation. It EXECUTES messages: cosmos-sdk's
	// runMsgs breaks out of its loop only when the mode is neither finalize nor
	// simulate (baseapp.go:1039), and its doc says so in words -- "Messages will
	// only be executed during simulation and DeliverTx". This is why a message
	// index can appear here and nowhere else on our paths.
	TxStageSimulate TxStage = "simulate"

	// TxStageCheckTx is an ABCI rejection reported inside a successful
	// BroadcastTx response. CheckTx does NOT execute messages, so a rejection
	// here can never carry a message index -- see parseMsgIndex's callers.
	TxStageCheckTx TxStage = "checktx"

	// TxStageBroadcast is a transport failure of the BroadcastTx call itself.
	// Nobody rejected anything: the request may or may not have reached the
	// node, which is why GRPCCode matters most here.
	TxStageBroadcast TxStage = "broadcast"
)

// nilCauseText is what Error() prints in place of a cause that is missing.
// A stage whose format needs the wrapped error but does not have one must not
// print the bare prefix: a truncated error reads as a complete one.
const nilCauseText = "<nil cause>"

// msgIndexNeedle is the only part of cosmos-sdk's two index-writing sites that
// both share. baseapp.go writes "failed to execute message; message index: %d"
// (:1052) and "failed to create message events; message index: %d" (:1058);
// anchoring on the longer prefix drops the second one silently.
const msgIndexNeedle = "message index: "

// TxRejection is the machine-readable form of a transaction rejection. It
// exists so that a caller can ask WHAT was rejected instead of matching
// substrings on a message that grows a wrapper at every frame.
//
// Error() is byte-identical to the fmt.Errorf it replaced at each of the three
// construction sites, so introducing it changes no log line and no existing
// classifier. That identity is the whole safety argument, and it only holds
// while Error() stays a pure function of these fields.
//
// TWO FIELDS CARRY NEARLY THE SAME STRING, and picking the wrong one is a
// failure no substring check can see: RawLog is the server's own text, while
// Error() for the simulate and broadcast stages is built from the wrapped
// error, whose text is "rpc error: code = X desc = <RawLog>". RawLog is NOT
// where Error() comes from.
//
// ABCICode/Codespace and GRPCCode are two different numbering systems and are
// kept in two differently typed fields on purpose. Neither is a classification
// key for x/proof errors: codes.FailedPrecondition alone covers the claim
// window check, a missing claim, a malformed bech32 address that is not even
// registered in x/proof, two distinct fee-deduction failures, and "proof not
// required" -- conditions that do not share a module of origin, with an
// irreversible downgrade hanging off the benign one. Classify those by text.
type TxRejection struct {
	// Stage is which of the three rejections this is. Read every other field
	// relative to it.
	Stage TxStage

	// TxHash is the transaction's hash where one exists: reported by the server
	// in checktx, reconstructed from the signed bytes in broadcast, and EMPTY in
	// simulate -- not because nothing was transmitted (the bytes did travel) but
	// because they are not the final transaction: the gas limit and fee are set
	// and the tx is re-signed after simulating.
	TxHash string

	// ABCICode and Codespace are the chain's own rejection codes, and exist only
	// in checktx. Zero everywhere else.
	ABCICode  uint32
	Codespace string

	// GRPCCode is the transport-level status. codes.OK in checktx, where the RPC
	// itself succeeded; always codes.Unknown in simulate, because the tx service
	// flattens every simulation failure to it; and the real value in broadcast,
	// which is the only stage where it distinguishes anything. No "has" flag is
	// needed because codes.OK is zero AND is the true statement for checktx.
	GRPCCode codes.Code

	// MsgIndex is which message of the batch failed, and HasMsgIndex is whether
	// there was an index at all. The pair is required: the ante handler runs in
	// simulate too and fails BEFORE any message executes, so a fee, nonce or TTL
	// failure arrives with no index, and a bare int would report that as
	// "message 0 failed" -- feeding an irreversible per-message downgrade.
	MsgIndex    int
	HasMsgIndex bool

	// RawLog is the text the SERVER produced, with cosmos's own wrappers and
	// without ours. See the type comment: this is not what Error() prints.
	RawLog string

	// wrapped keeps the error chain that the fmt.Errorf %w used to provide. It
	// is nil in checktx, which used %s and chained nothing.
	wrapped error
}

// Error reproduces, byte for byte, the message each construction site produced
// before this type existed.
func (r *TxRejection) Error() string {
	if r == nil {
		// Same class as the default branch below: exported fields make this
		// type constructible from anywhere, and a nil *TxRejection inside an
		// error interface is as easy to produce as a zero Stage. No constructor
		// yields one; that is not the same as it being unreachable.
		return "<nil tx rejection>"
	}

	switch r.Stage {
	case TxStageSimulate:
		return "simulation failed: " + r.causeText()
	case TxStageBroadcast:
		return "failed to broadcast transaction: " + r.causeText()
	case TxStageCheckTx:
		return fmt.Sprintf("CheckTx failed (code %d): %s", r.ABCICode, r.RawLog)
	default:
		// Live branch, not defensive. errors.As forces the fields to be
		// exported for outside consumers to read them, and exported fields make
		// TxRejection{} constructible from any package with a zero Stage. An
		// unexported constructor narrows accidents inside this package; it does
		// not close the set.
		return fmt.Sprintf("tx rejected at unknown stage %q: %s", r.Stage, r.RawLog)
	}
}

func (r *TxRejection) causeText() string {
	if r.wrapped == nil {
		return nilCauseText
	}
	return r.wrapped.Error()
}

// Unwrap keeps errors.Is and errors.As working down the chain, exactly as the
// %w verbs did. It returns nil for checktx, which never chained anything.
func (r *TxRejection) Unwrap() error {
	if r == nil {
		return nil
	}
	return r.wrapped
}

// newSimulateRejection captures a failed Simulate AT THE CALL SITE, which is not
// a convenience: status.FromError takes a different branch for a WRAPPED status
// error and overwrites the message with err.Error(), so a late capture silently
// returns our own prefixes instead of the server's text.
func newSimulateRejection(err error) *TxRejection {
	rawLog := statusMessage(err)
	idx, hasIdx := parseMsgIndex(rawLog)
	return &TxRejection{
		Stage:       TxStageSimulate,
		GRPCCode:    status.Code(err),
		MsgIndex:    idx,
		HasMsgIndex: hasIdx,
		RawLog:      rawLog,
		wrapped:     err,
	}
}

// newCheckTxRejection reads an ABCI rejection out of a successful broadcast.
// It does NOT parse a message index: CheckTx stops before executing messages,
// so an index appearing in this RawLog came from somewhere else.
func newCheckTxRejection(res *cosmostypes.TxResponse) *TxRejection {
	return &TxRejection{
		Stage:     TxStageCheckTx,
		TxHash:    res.TxHash,
		ABCICode:  res.Code,
		Codespace: res.Codespace,
		GRPCCode:  codes.OK,
		RawLog:    res.RawLog,
	}
}

// txHashOf returns the hash the chain will report for these encoded bytes.
//
// The node derives it as the uppercase hex of the full sha256 of exactly these
// bytes (cometbft types.Tx.Hash is tmhash.Sum; cosmos formats it with %X).
// tmhash.SumTruncated is the neighbour that yields 20 bytes -- plausible and
// wrong.
//
// It is a function rather than two copies of the expression because the answer
// has to be IDENTICAL in both places that need it: the rejection below, which
// recovers the hash of a send that got no answer, and the signing path, which
// needs it before sending at all. Two copies that drifted would not fail
// loudly -- they would name two different transactions and each look right.
func txHashOf(txBytes []byte) string {
	return strings.ToUpper(hex.EncodeToString(tmhash.Sum(txBytes)))
}

// newBroadcastRejection covers a BroadcastTx that never returned a response.
// The hash is recovered rather than lost.
//
// The caller's return value is unchanged; the hash is recoverable through
// errors.As, not returned.
func newBroadcastRejection(err error, txBytes []byte) *TxRejection {
	return &TxRejection{
		Stage:    TxStageBroadcast,
		TxHash:   txHashOf(txBytes),
		GRPCCode: status.Code(err),
		RawLog:   statusMessage(err),
		wrapped:  err,
	}
}

// statusMessage extracts the server's text from a gRPC error. A non-status
// error yields its own Error(), so the result is never empty for a non-nil err.
func statusMessage(err error) string {
	if err == nil {
		return ""
	}
	s, _ := status.FromError(err)
	return s.Message()
}

// parseMsgIndex finds the message index cosmos-sdk writes into a failed
// execution. Returns false when there is none, which is a different fact from
// index zero.
func parseMsgIndex(rawLog string) (int, bool) {
	// FIRST match, and that is load-bearing rather than incidental.
	// errorsmod.Wrapf renders as "<wrapper>: <cause>", so the index cosmos
	// wrote is always LEFT of anything the server's own text may contain. Using
	// the last match would read a number out of the content, and an irreversible
	// per-message downgrade hangs off this value.
	at := strings.Index(rawLog, msgIndexNeedle)
	if at < 0 {
		return 0, false
	}
	rest := rawLog[at+len(msgIndexNeedle):]

	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}

	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}
