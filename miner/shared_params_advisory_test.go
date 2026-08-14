//go:build test

package miner

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
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

// TestCheckSharedParamsAdvisory_BundledGenesisIsClean reads the ACTUAL
// tilt/config/genesis.json rather than a hand-copied literal. A test that asserts on
// its own copy of the values cannot notice the file being reverted or drifting, which
// is the only thing worth pinning here — the localnet shipped params the protocol
// would reject until 2026-08-14.
func TestCheckSharedParamsAdvisory_BundledGenesisIsClean(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "tilt", "config", "genesis.json"))
	require.NoError(t, err, "bundled localnet genesis must be readable")

	var genesis struct {
		AppState struct {
			Shared struct {
				Params sharedtypes.Params `json:"params"`
			} `json:"shared"`
		} `json:"app_state"`
	}
	require.NoError(t, json.Unmarshal(raw, &genesis), "genesis.json must parse")

	params := genesis.AppState.Shared.Params

	// Guard against the decode silently yielding a zero struct, which would make the
	// assertion below pass for the wrong reason.
	require.NotZero(t, params.NumBlocksPerSession, "genesis shared params failed to decode")

	require.NoError(t, CheckSharedParamsAdvisory(&params),
		"tilt/config/genesis.json must satisfy poktroll's own validation "+
			"(num_blocks_per_session=%d, application_unbonding_period_sessions=%d, "+
			"cumulative settlement blocks=%d)",
		params.NumBlocksPerSession, params.ApplicationUnbondingPeriodSessions,
		sharedtypes.GetSessionEndToProofWindowCloseBlocks(&params))
}

func TestCheckSharedParamsAdvisory_NilIsNotAnAdvisory(t *testing.T) {
	require.NoError(t, CheckSharedParamsAdvisory(nil))
}

// TestLogSharedParamsAdvisory_EmitsOnlyOnViolation exercises the function that actually
// runs in production, not just the predicate underneath it. Without this, a refactor
// that drops the LogSharedParamsAdvisory call — or inverts its early return — breaks
// nothing in the suite.
func TestLogSharedParamsAdvisory_EmitsOnlyOnViolation(t *testing.T) {
	tests := []struct {
		name         string
		params       *sharedtypes.Params
		wantAdvisory bool
	}{
		{"nil params stay silent", nil, false},
		{"mainnet stays silent", mainnetSharedParams(), false},
		{"violating localnet genesis warns", localnetGenesisSharedParams(), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			// logging.Logger is an alias for zerolog.Logger, so a buffer-backed logger
			// captures exactly what production would emit.
			logger := zerolog.New(&buf).Level(zerolog.WarnLevel)

			LogSharedParamsAdvisory(logger, tt.params)

			out := buf.String()
			if !tt.wantAdvisory {
				require.Empty(t, out, "no advisory must be emitted for satisfying params")
				return
			}

			require.Contains(t, out, "ValidateBasic",
				"the advisory must say which validation failed")
			require.Contains(t, out, "ApplicationUnbondingPeriodSessions",
				"the wrapped error must name the failing invariant")
			require.Contains(t, out, "session_end_to_proof_window_close_blocks",
				"the advisory must carry the settlement window it was compared against")
		})
	}
}
