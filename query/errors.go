package query

import (
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
