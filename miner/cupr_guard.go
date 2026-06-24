package miner

// isClaimCUPRConsistent reports whether an SMST root's compute-units sum matches
// num_relays * compute_units_per_relay — the equality poktroll enforces at
// claim-create (x/proof) and again at settlement (x/tokenomics), both reading
// the service's CUPR at the LATEST height.
//
// A false result means the session's relays were metered against a CUPR that
// differs from the one the chain will validate against (a mid-session, or
// pre-claim, service CUPR change). Such a claim is rejected on-chain and would
// otherwise be retried every block until the window closes.
//
// cupr == 0 means the current CUPR is unknown (query failed / service missing);
// it returns true so callers fail OPEN and never drop a claim they cannot prove
// is doomed.
func isClaimCUPRConsistent(smstSum, smstCount, cupr uint64) bool {
	if cupr == 0 {
		return true
	}
	return smstSum == smstCount*cupr
}
