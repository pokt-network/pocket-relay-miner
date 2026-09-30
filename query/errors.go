package query

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// IsEntityNotFound reports whether err is the chain's DEFINITIVE answer that the
// queried entity does not exist, as opposed to a failure to obtain an answer at all.
//
// POLICY — a gRPC NotFound status is the only signal that qualifies. Every caller in
// the miner, the relayer command and this package uses it. The two equivalent inline
// checks left in cache/ are deliberate: that package does not depend on query today,
// and a new package edge is not worth removing two lines of duplication.
//
// Callers use this to decide whether an entity is absent on-chain, and several of
// those decisions are terminal and cost money (see the pre-proof guard in
// miner.OnSessionsNeedProof: "no claim on-chain" skips the proof, marks the session
// claim_missing, and the claim then expires into a ProofMissingPenalty). Such a
// decision must never be reached from a transport or node failure, so anything that
// is not an explicit NotFound must be treated as "unknown" and the caller must FAIL
// OPEN — the same policy the claim-side CUPR guard already follows ("never drop a
// claim we cannot prove is doomed").
//
// Why there is no message-substring fallback, deliberately:
//
//   - It is unnecessary. poktroll answers a missing entity with an explicit
//     status.Error(codes.NotFound, ...) (e.g. x/proof/keeper/query_claim.go returns
//     codes.NotFound wrapping ErrProofClaimNotFound), and status.FromError unwraps
//     fmt.Errorf("%w")-wrapped errors, so the query layer's wrapping does not hide it.
//   - It is dangerous. Matching on "not found" anywhere in the message also captures
//     transport and node failures that merely mention it — "header not found" (a
//     CometBFT error for a height a node does not have or has pruned), "peer not
//     found", "route not found" — turning a transient failure into a terminal,
//     money-losing decision. The set of such messages is defined by the node, the
//     proxy and the transport, not by us, so it cannot be enumerated or bounded.
func IsEntityNotFound(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	return ok && st.Code() == codes.NotFound
}

// heightNotYetAvailableTexts are the node's words for "I do not have that height
// yet". Both are bare error strings with no code of their own: CometBFT answers
// the generic JSON-RPC -32603 around the first (rpc/core/env.go getHeight), and
// poktroll flattens the second into codes.Internal (x/session keeper,
// session_hydrator.go). The code alone would also match real internal errors,
// so the text is the only signal.
var heightNotYetAvailableTexts = []string{
	// CometBFT, any at-height RPC (Block, BlockResults, ...):
	// "height 690363 must be less than or equal to the current blockchain height 690362"
	"must be less than or equal to the current blockchain height",
	// poktroll session query, the block is stored but its state not yet committed:
	// "block height 100 is ahead of the last committed block height 99"
	"is ahead of the last committed block height",
}

// IsHeightNotYetAvailable reports whether err is a node saying it does not have
// the requested height YET: a retry-later, never a failure. A node announces a
// height (a websocket event, Status) before it can serve the data at it, and
// behind a load balancer the next call can land on a node one block behind.
func IsHeightNotYetAvailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, text := range heightNotYetAvailableTexts {
		if strings.Contains(msg, text) {
			return true
		}
	}
	return false
}

// notYetRetryBaseDelay and notYetRetryMaxDelay pace RetryWhileHeightNotYet.
const (
	notYetRetryBaseDelay = 250 * time.Millisecond
	notYetRetryMaxDelay  = 1 * time.Second
)

// RetryWhileHeightNotYet calls fn until it succeeds, fails with an error that
// is not IsHeightNotYetAvailable, or ctx ends. It is the one retry loop for a
// node's "not yet": every at-height read that can meet it goes through here.
func RetryWhileHeightNotYet[V any](ctx context.Context, fn func() (V, error)) (V, error) {
	delay := notYetRetryBaseDelay
	for {
		v, err := fn()
		if err == nil || !IsHeightNotYetAvailable(err) {
			return v, err
		}
		select {
		case <-ctx.Done():
			var zero V
			return zero, fmt.Errorf("%w (last answer: %w)", ctx.Err(), err)
		case <-time.After(delay):
		}
		delay = min(delay*2, notYetRetryMaxDelay)
	}
}
