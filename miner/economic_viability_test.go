//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/query"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// mockServiceDifficultyClient is the minimal ServiceDifficultyClient mock
// needed by the economic-viability path (getClaimReward). It echoes back the
// requested service ID so the downstream validation in Claim.GetClaimeduPOKT
// (which asserts claim.ServiceId == difficulty.ServiceId) is satisfied.
type mockServiceDifficultyClient struct {
	targetHash []byte
}

func (m *mockServiceDifficultyClient) GetServiceRelayDifficultyAtHeight(
	_ context.Context, serviceID string, _ int64,
) (servicetypes.RelayMiningDifficulty, error) {
	return servicetypes.RelayMiningDifficulty{
		ServiceId:  serviceID,
		TargetHash: m.targetHash,
	}, nil
}

var _ query.ServiceDifficultyClient = (*mockServiceDifficultyClient)(nil)

// TestGetClaimReward_CanonicalFormula pins the production reward calculation
// (LifecycleCallback.getClaimReward → poktroll's Claim.GetClaimeduPOKT) against
// the chain formula:
//
//	reward_uPOKT = floor(numComputeUnits × difficultyMultiplier × CUTTM / granularity)
//
// The cases below cover the regressions that the *previous* local shadow
// implementation (`computeClaimRewardRat`) was written to prevent. We now
// exercise the upstream formula directly, so if upstream ever reintroduces
// premature big.Int truncation (the 2026-03 bug), these tests will catch it.
//
// How the test constructs a Claim:
//   - Uses real RedisSMSTManager + miniredis to build a tree.
//   - Inserts ONE relay with weight=totalComputeUnits (the SMST sum encodes CU).
//   - Flushes to produce a real RootHash that Claim.GetClaimeduPOKT parses.
//
// This guarantees we are testing the exact root-hash encoding the production
// flow produces, not a mocked shortcut.
func TestGetClaimReward_CanonicalFormula(t *testing.T) {
	// Base difficulty → difficulty multiplier == 1 → deterministic math.
	baseTargetHash := protocol.BaseRelayDifficultyHashBz

	tests := []struct {
		name        string
		totalCU     uint64 // inserted as a single relay's weight
		cuttm       uint64
		granularity uint64
		wantUpokt   int64 // expected Coin.Amount.Int64() after floor()
	}{
		// Tilt defaults: cuttm=1, granularity=1_000_000.
		// Single 1000-CU relay → 1000 × 1 / 1_000_000 = 0.001 uPOKT → floors to 0.
		// This is the sub-uPOKT edge case. The chain's own formula truncates
		// here, which is correct — but the intermediate math MUST stay in Rat
		// so a 1-relay session isn't mis-counted.
		{"tilt sub-uPOKT (1 relay, 1000 CU)", 1000, 1, 1_000_000, 0},
		// 1_000_000 CU → exactly 1 uPOKT.
		{"tilt exactly one uPOKT", 1_000_000, 1, 1_000_000, 1},
		// 2_001_000 CU → 2.001 uPOKT → 2 (floor).
		{"tilt just over 2 uPOKT", 2_001_000, 1, 1_000_000, 2},
		// Mainnet-ish: cuttm=77033, granularity=1_000_000.
		// 10 CU → 10 × 77033 / 1_000_000 = 0.77033 uPOKT → 0.
		{"mainnet sub-uPOKT (10 CU)", 10, 77033, 1_000_000, 0},
		// 100_000 CU → 100_000 × 77033 / 1_000_000 = 7703.3 uPOKT → 7703.
		{"mainnet multi uPOKT (100k CU)", 100_000, 77033, 1_000_000, 7703},
		// Granularity coerced to 1 inside upstream when zero is rejected by
		// SetFrac64 — we guard only the non-zero case here, matching the
		// production constraint that genesis always sets granularity≥1.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			suite := newEconomicViabilityHarness(t, baseTargetHash, tt.cuttm, tt.granularity)
			defer suite.cleanup()

			supplierAddr := "pokt1test_getclaimreward"
			sessionID := "session_" + tt.name
			serviceID := "svc-econ-test"

			mgr := suite.createManager(supplierAddr)

			// Insert ONE relay whose weight encodes the total compute units.
			require.NoError(t,
				mgr.UpdateTree(suite.ctx, sessionID, []byte("relay-key"), []byte("relay-value"), tt.totalCU),
				"UpdateTree")

			rootHash, err := mgr.FlushTree(suite.ctx, sessionID)
			require.NoError(t, err, "FlushTree")
			require.NotEmpty(t, rootHash, "FlushTree should return a non-empty root hash")

			claim := &prooftypes.Claim{
				SupplierOperatorAddress: supplierAddr,
				SessionHeader: &sessiontypes.SessionHeader{
					ServiceId:               serviceID,
					SessionStartBlockHeight: 1,
					SessionEndBlockHeight:   10,
					SessionId:               sessionID,
				},
				RootHash: rootHash,
			}

			lc := suite.newLifecycleCallback(supplierAddr)
			coin, err := lc.getClaimReward(suite.ctx, claim, serviceID)
			require.NoError(t, err, "getClaimReward must not error on valid inputs")
			require.Equalf(t, sdk.NewInt64Coin("upokt", tt.wantUpokt), coin,
				"reward mismatch: want %d uPOKT, got %s", tt.wantUpokt, coin.String())
		})
	}
}

// TestGetClaimReward_PropagatesQueryErrors verifies getClaimReward surfaces
// failures from its two dependencies (difficulty client, shared client)
// instead of silently returning a zero coin, which would previously mask
// misconfiguration as "unprofitable" and silently drop claims.
func TestGetClaimReward_PropagatesQueryErrors(t *testing.T) {
	supplierAddr := "pokt1test_getclaimreward_errs"
	serviceID := "svc-err"

	t.Run("nil proof checker -> error", func(t *testing.T) {
		lc := &LifecycleCallback{
			logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
			sharedClient: &mockSharedQueryClient{},
		}
		_, err := lc.getClaimReward(context.Background(), &prooftypes.Claim{
			SupplierOperatorAddress: supplierAddr,
			SessionHeader: &sessiontypes.SessionHeader{
				ServiceId: serviceID,
			},
		}, serviceID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "proof checker not available")
	})

	t.Run("nil difficulty client -> error", func(t *testing.T) {
		lc := &LifecycleCallback{
			logger:       logging.NewLoggerFromConfig(logging.DefaultConfig()),
			sharedClient: &mockSharedQueryClient{},
			proofChecker: NewProofRequirementChecker(
				logging.NewLoggerFromConfig(logging.DefaultConfig()),
				nil, nil, nil, // nil difficulty client
			),
		}
		_, err := lc.getClaimReward(context.Background(), &prooftypes.Claim{
			SupplierOperatorAddress: supplierAddr,
			SessionHeader: &sessiontypes.SessionHeader{
				ServiceId: serviceID,
			},
		}, serviceID)
		require.Error(t, err)
		require.Contains(t, err.Error(), "difficulty client not available")
	})
}

// --- test harness ---

// economicViabilityHarness wires a real miniredis-backed RedisSMSTManager and
// mock query clients so the test can build legitimate Claim.RootHash bytes
// and exercise the full getClaimReward code path.
type economicViabilityHarness struct {
	t            *testing.T
	ctx          context.Context
	miniRedis    *miniredis.Miniredis
	redisClient  *redisutil.Client
	sharedParams *sharedtypes.Params
	targetHash   []byte
}

func newEconomicViabilityHarness(
	t *testing.T,
	targetHash []byte,
	cuttm, granularity uint64,
) *economicViabilityHarness {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err, "miniredis")

	ctx := context.Background()
	client, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err, "redis client")

	params := &sharedtypes.Params{
		ComputeUnitsToTokensMultiplier: cuttm,
		ComputeUnitCostGranularity:     granularity,
		NumBlocksPerSession:            10,
		ClaimWindowOpenOffsetBlocks:    1,
		ClaimWindowCloseOffsetBlocks:   8,
		ProofWindowOpenOffsetBlocks:    0,
		ProofWindowCloseOffsetBlocks:   8,
	}
	return &economicViabilityHarness{
		t:            t,
		ctx:          ctx,
		miniRedis:    mr,
		redisClient:  client,
		sharedParams: params,
		targetHash:   targetHash,
	}
}

func (h *economicViabilityHarness) cleanup() {
	if h.redisClient != nil {
		_ = h.redisClient.Close()
	}
	if h.miniRedis != nil {
		h.miniRedis.Close()
	}
}

func (h *economicViabilityHarness) createManager(supplierAddr string) *RedisSMSTManager {
	return NewRedisSMSTManager(zerolog.Nop(), h.redisClient, RedisSMSTManagerConfig{
		SupplierAddress: supplierAddr,
		CacheTTL:        0,
	})
}

func (h *economicViabilityHarness) newLifecycleCallback(supplierAddr string) *LifecycleCallback {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	proofChecker := NewProofRequirementChecker(
		logger,
		nil, // proofClient unused by getClaimReward
		nil, // sharedClient unused by getClaimReward (LifecycleCallback.sharedClient is the one called)
		&mockServiceDifficultyClient{targetHash: h.targetHash},
	)
	lc := &LifecycleCallback{
		logger:       logger,
		config:       DefaultLifecycleCallbackConfig(),
		sharedClient: &mockSharedQueryClient{params: h.sharedParams},
		proofChecker: proofChecker,
	}
	lc.config.SupplierAddress = supplierAddr
	return lc
}

// Interface assertion: mockSharedQueryClient already implements SharedQueryClient
// (defined in claim_pipeline_test.go, same build tag).
var _ pocktclient.SharedQueryClient = (*mockSharedQueryClient)(nil)
