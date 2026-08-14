package miner

import (
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// sharedParamsAdvisoryTimeout caps the one chain read the startup advisory makes.
// Nothing downstream consumes the result, so a slow node must not keep a worker
// slot busy for the full (operator-configurable) query timeout.
const sharedParamsAdvisoryTimeout = 10 * time.Second

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
// Checked once at startup, deliberately, not re-evaluated on every params change:
// both MsgUpdateParams (x/shared/keeper/msg_update_params.go:19) and MsgUpdateParam
// (msg_server_update_param.go:115) run ValidateBasic before writing, so governance
// cannot move a live chain INTO a violating state. Genesis is the only way in, and
// genesis is fixed for the process lifetime. The remaining case — a dependency bump
// that adds an invariant — arrives with a new binary, hence a restart, hence a run.
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

	// The message stays generic on purpose: ValidateBasic enforces ~15 unrelated
	// invariants, so naming one consequence would send an operator to inspect a
	// parameter that is not the one that failed. The wrapped error names it instead.
	logger.Warn().
		Err(err).
		Uint64("num_blocks_per_session", params.GetNumBlocksPerSession()).
		Uint64("application_unbonding_period_sessions", params.GetApplicationUnbondingPeriodSessions()).
		Uint64("supplier_unbonding_period_sessions", params.GetSupplierUnbondingPeriodSessions()).
		Int64("session_end_to_proof_window_close_blocks", sharedtypes.GetSessionEndToProofWindowCloseBlocks(params)).
		Msg("chain shared params do not satisfy poktroll's own Params.ValidateBasic — " +
			"see the error field for the failing invariant; if it is an unbonding period, " +
			"stake can be withdrawn before the claims covering it settle")
}
