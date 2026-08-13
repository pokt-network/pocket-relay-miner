package relayer

// Bounds for a plausibly-servable session header, used to cheaply reject
// obviously-bogus client-supplied heights BEFORE any at-height chain query.
//
// These are deliberately generous: a false rejection drops a legitimate relay
// (certain revenue loss), while a slightly-loose bound only lets an attacker
// vary heights within a window around the current height. Real sessions are tens
// of blocks (num_blocks_per_session has ranged ~10-60 on-chain) and grace periods
// a fraction of that, so a 10k-block window never rejects a real relay yet
// collapses the attacker-usable height space from the whole int64 range to a
// bounded band that the per-height query cache absorbs.
const (
	// maxPlausibleSessionLengthBlocks caps a session's block span.
	maxPlausibleSessionLengthBlocks = int64(10_000)

	// maxSessionLookbackBlocks caps how far in the past a servable session's end
	// height may be (beyond any conceivable grace window).
	maxSessionLookbackBlocks = int64(10_000)

	// maxSessionLookaheadBlocks caps how far in the future a session's start height
	// may be, absorbing the relayer's block-event lag and freshly-opened sessions.
	maxSessionLookaheadBlocks = int64(10_000)
)

// sessionHeightsPlausible reports whether a client-supplied session header could
// belong to a session this relayer can serve, using only integer comparisons —
// no chain query. It runs before the relay meter's at-height reads, which for an
// unauthenticated request would otherwise let arbitrary distinct heights each
// drive a fresh full-node query (a pre-signature amplification surface). A
// legitimate relay — active session or grace-period — always passes.
//
// arrivalHeight <= 0 means the relayer has not yet observed a block (boot window):
// the heights cannot be judged against a reference, so the header is allowed
// through and later validation decides.
func sessionHeightsPlausible(startHeight, endHeight, arrivalHeight int64) bool {
	// Basic structural sanity (also enforced by ValidateBasic, but that runs
	// AFTER the eager meter, so re-check it here on the pre-meter path).
	if startHeight <= 0 || endHeight <= startHeight {
		return false
	}
	if endHeight-startHeight > maxPlausibleSessionLengthBlocks {
		return false
	}

	// No block seen yet: cannot bound against a reference height.
	if arrivalHeight <= 0 {
		return true
	}

	// Start cannot be meaningfully in the future; end cannot be so far in the
	// past that no grace window could still accept the relay.
	if startHeight > arrivalHeight+maxSessionLookaheadBlocks {
		return false
	}
	if endHeight < arrivalHeight-maxSessionLookbackBlocks {
		return false
	}

	return true
}
