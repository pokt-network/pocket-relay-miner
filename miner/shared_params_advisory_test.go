//go:build test

package miner

import (
	"testing"

	"github.com/stretchr/testify/require"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// mainnetSharedParams / betaSharedParams are the values live on 2026-08-14, read from
// the public REST endpoints. They are here so a future dependency bump that tightens
// ValidateBasic fails this file instead of surprising an operator at startup.
func mainnetSharedParams() *sharedtypes.Params {
	return &sharedtypes.Params{
		NumBlocksPerSession:                20,
		GracePeriodEndOffsetBlocks:         10,
		ClaimWindowOpenOffsetBlocks:        11,
		ClaimWindowCloseOffsetBlocks:       10,
		ProofWindowOpenOffsetBlocks:        1,
		ProofWindowCloseOffsetBlocks:       10,
		SupplierUnbondingPeriodSessions:    1429,
		ApplicationUnbondingPeriodSessions: 3,
		GatewayUnbondingPeriodSessions:     1,
		ComputeUnitsToTokensMultiplier:     100,
		ComputeUnitCostGranularity:         1,
	}
}

func betaSharedParams() *sharedtypes.Params {
	p := mainnetSharedParams()
	p.SupplierUnbondingPeriodSessions = 86
	p.ApplicationUnbondingPeriodSessions = 2
	return p
}

// localnetGenesisSharedParams reproduces tilt/config/genesis.json BEFORE the fix:
// application_unbonding_period_sessions=1 at num_blocks_per_session=10 gives a
// 10-block unbonding period against a 17-block claim+proof settlement window.
func localnetGenesisSharedParams() *sharedtypes.Params {
	return &sharedtypes.Params{
		NumBlocksPerSession:                10,
		GracePeriodEndOffsetBlocks:         1,
		ClaimWindowOpenOffsetBlocks:        1,
		ClaimWindowCloseOffsetBlocks:       8,
		ProofWindowOpenOffsetBlocks:        0,
		ProofWindowCloseOffsetBlocks:       8,
		SupplierUnbondingPeriodSessions:    20,
		ApplicationUnbondingPeriodSessions: 1,
		GatewayUnbondingPeriodSessions:     1,
		ComputeUnitsToTokensMultiplier:     100,
		ComputeUnitCostGranularity:         1,
	}
}

// TestCheckSharedParamsAdvisory_LiveNetworksAreClean pins that the networks we
// actually run against satisfy the invariant, so a warning in production means
// something really changed rather than the check being noisy by construction.
func TestCheckSharedParamsAdvisory_LiveNetworksAreClean(t *testing.T) {
	for name, params := range map[string]*sharedtypes.Params{
		"mainnet": mainnetSharedParams(),
		"beta":    betaSharedParams(),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, CheckSharedParamsAdvisory(params),
				"live %s shared params must satisfy poktroll's own validation", name)
		})
	}
}

// TestCheckSharedParamsAdvisory_DetectsShortApplicationUnbonding is the defect this
// advisory exists for: an application whose unbonding period is shorter than the
// window in which its sessions are still claimed, proved and settled can withdraw
// before the supplier's claims settle.
func TestCheckSharedParamsAdvisory_DetectsShortApplicationUnbonding(t *testing.T) {
	params := localnetGenesisSharedParams()

	// Precondition, so the test fails for the reason it claims: unbonding really is
	// shorter than the settlement window.
	unbondingBlocks := int64(params.ApplicationUnbondingPeriodSessions * params.NumBlocksPerSession)
	cumulative := sharedtypes.GetSessionEndToProofWindowCloseBlocks(params)
	require.Less(t, unbondingBlocks, cumulative,
		"fixture must actually violate the invariant (%d unbonding blocks vs %d settlement blocks)",
		unbondingBlocks, cumulative)

	err := CheckSharedParamsAdvisory(params)
	require.Error(t, err, "params the protocol would reject must be reported")
	require.Contains(t, err.Error(), "ApplicationUnbondingPeriodSessions",
		"the advisory must name the offending parameter so an operator can act on it")
}

// TestCheckSharedParamsAdvisory_FixedLocalnetIsClean pins the fix applied to
// tilt/config/genesis.json (application_unbonding_period_sessions 1 -> 2).
func TestCheckSharedParamsAdvisory_FixedLocalnetIsClean(t *testing.T) {
	params := localnetGenesisSharedParams()
	params.ApplicationUnbondingPeriodSessions = 2

	require.NoError(t, CheckSharedParamsAdvisory(params),
		"2 sessions x 10 blocks = 20 blocks must clear the 17-block settlement window")
}

func TestCheckSharedParamsAdvisory_NilIsNotAnAdvisory(t *testing.T) {
	require.NoError(t, CheckSharedParamsAdvisory(nil))
}
