package miner

import "context"

// ClaimCUPRQueryClient resolves the compute_units_per_relay that was effective
// for a service at a block height. Satisfied by poktroll's client.ServiceQueryClient.
type ClaimCUPRQueryClient interface {
	GetServiceComputeUnitsPerRelayAtHeight(ctx context.Context, serviceID string, blockHeight int64) (uint64, error)
}

// isClaimCUPRConsistent reports whether an SMST root's compute-units sum matches
// num_relays * compute_units_per_relay — the equality poktroll enforces at
// claim-create (x/proof) and again at settlement (x/tokenomics).
//
// Callers MUST pass the CUPR effective at the session's START height. Both chain
// checks resolve it there, so a CUPR change after the session ended cannot make a
// claim invalid — comparing against the LATEST value instead skips claims the
// chain would have paid.
//
// A false result means the session's relays were metered against a CUPR that
// differs from the one the chain will validate against — a mid-session service
// CUPR change that split the tree into two weights. Such a claim is rejected
// on-chain and would otherwise be retried every block until the window closes.
//
// cupr == 0 means the session-start CUPR is unknown (query failed / service
// missing); it returns true so callers fail OPEN and never drop a claim they
// cannot prove is doomed.
func isClaimCUPRConsistent(smstSum, smstCount, cupr uint64) bool {
	if cupr == 0 {
		return true
	}
	return smstSum == smstCount*cupr
}

// evaluateClaimCUPRGuard decides whether a claim may be submitted, by comparing
// the SMST's compute-unit sum against the CUPR the chain will price it with.
//
// The returned cupr is the value compared against, for logging. err is returned
// (with allowed=true) when the CUPR could not be resolved: this guard discards a
// whole session's revenue terminally, so it must only fire on a mismatch it can
// actually prove.
//
// The query is deliberately NOT cache-busted. CUPR at a past height is immutable,
// so the query layer's per-(service, height) entry is always correct — and the
// previous implementation's forced invalidation existed only to serve the LATEST
// value, which is the wrong comparand.
func evaluateClaimCUPRGuard(
	ctx context.Context,
	client ClaimCUPRQueryClient,
	serviceID string,
	sessionStartHeight int64,
	smstSum, smstCount uint64,
) (allowed bool, cupr uint64, err error) {
	if client == nil {
		return true, 0, nil
	}

	cupr, err = client.GetServiceComputeUnitsPerRelayAtHeight(ctx, serviceID, sessionStartHeight)
	if err != nil {
		return true, 0, err
	}

	return isClaimCUPRConsistent(smstSum, smstCount, cupr), cupr, nil
}
