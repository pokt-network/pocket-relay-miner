//go:build test

package relayer

import (
	"context"
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// fakeSessionClient implements client.SessionQueryClient by embedding the
// interface (so unused methods exist) and overriding GetParams to return a
// configurable NumSuppliersPerSession.
type fakeSessionClient struct {
	client.SessionQueryClient
	numSuppliers uint64
}

func (f *fakeSessionClient) GetParams(_ context.Context) (*sessiontypes.Params, error) {
	return &sessiontypes.Params{NumSuppliersPerSession: f.numSuppliers}, nil
}

// TestGetSessionParams_ReflectsChangedNumSuppliers is the regression test for
// the frozen session-params path: getSessionParams must reflect a governance
// change to NumSuppliersPerSession, not serve a value frozen in the
// ha:params:session Redis key (whose only proactive writer is dead code).
func TestGetSessionParams_ReflectsChangedNumSuppliers(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	ctx := context.Background()

	redisClient, err := redisutil.NewClient(ctx, redisutil.ClientConfig{
		URL: fmt.Sprintf("redis://%s", mr.Addr()),
	})
	require.NoError(t, err)
	defer func() { _ = redisClient.Close() }()

	sess := &fakeSessionClient{numSuppliers: 2}
	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		&fakeAppClient{addr: "pokt1app"},
		nil, // sharedClient
		sess,
		nil, // blockClient
		&fakeSharedParamCache{params: &sharedtypes.Params{ComputeUnitsToTokensMultiplier: 1, ComputeUnitCostGranularity: 1}},
		nil, // serviceCache
		staticServiceFactor{f: 1},
		RelayMeterConfig{RedisKeyPrefix: "ha"},
	)

	p1, err := meter.getSessionParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(2), p1.NumSuppliersPerSession)

	// Governance changes NumSuppliersPerSession mid-flight.
	sess.numSuppliers = 4

	p2, err := meter.getSessionParams(ctx)
	require.NoError(t, err)
	require.Equal(t, uint64(4), p2.NumSuppliersPerSession,
		"getSessionParams must read the live session client, not a frozen Redis copy")
}
