package miner

import (
	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// CheckSharedParamsAdvisory reports why the chain's shared params fail poktroll's
// own Params.ValidateBasic, or nil when they satisfy it.
//
// A chain can run params that the protocol would refuse to move it INTO: genesis is
// applied without ValidateBasic, while MsgUpdateParams enforces it. So an operator
// can be connected to a chain whose configuration violates an invariant the protocol
// considers mandatory, and nothing on that chain will ever say so.
//
// The invariant that matters to a relay miner is the unbonding one
// (validateApplicationUnbondingPeriodIsGreaterThanCumulativeProofWindowCloseBlocks):
// an application's unbonding period must outlast the window in which its sessions are
// still being claimed, proved and settled. When it does not, an application can unbond
// and withdraw its stake before the supplier's claims for it settle.
//
// Delegating to ValidateBasic rather than restating the rules means any invariant
// poktroll adds later is picked up on the next dependency bump.
//
// Verified 2026-08-14 against live values: mainnet (num_blocks_per_session=20,
// application_unbonding_period_sessions=3 → 60 blocks vs 32 cumulative) and beta
// (=2 → 40 vs 32) both satisfy it. The localnet genesis bundled in tilt/config does
// not, which is how this check was found.
func CheckSharedParamsAdvisory(params *sharedtypes.Params) error {
	if params == nil {
		return nil
	}
	return params.ValidateBasic()
}

// LogSharedParamsAdvisory emits a startup warning when the chain's shared params do
// not satisfy poktroll's own validation.
//
// Advisory only, never fatal: these are the chain's parameters, not ours, and an
// operator cannot fix them. Refusing to start would strand a supplier on a chain it
// does not control, which is strictly worse than serving with a warning on record.
func LogSharedParamsAdvisory(logger logging.Logger, params *sharedtypes.Params) {
	err := CheckSharedParamsAdvisory(params)
	if err == nil {
		return
	}

	logger.Warn().
		Err(err).
		Uint64("num_blocks_per_session", params.GetNumBlocksPerSession()).
		Uint64("application_unbonding_period_sessions", params.GetApplicationUnbondingPeriodSessions()).
		Uint64("supplier_unbonding_period_sessions", params.GetSupplierUnbondingPeriodSessions()).
		Int64("session_end_to_proof_window_close_blocks", sharedtypes.GetSessionEndToProofWindowCloseBlocks(params)).
		Msg("chain shared params do not satisfy poktroll's own validation — " +
			"claims may settle against an application that already finished unbonding; " +
			"this chain was configured at genesis with values MsgUpdateParams would reject")
}
