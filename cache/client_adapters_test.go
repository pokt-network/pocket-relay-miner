//go:build test

package cache

import (
	"context"
	"errors"
	"testing"

	cosmosmath "cosmossdk.io/math"
	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	apptypes "github.com/pokt-network/poktroll/x/application/types"
	"github.com/stretchr/testify/require"
)

// stubAppParamsClient is a minimal client.ApplicationQueryClient used to verify
// that cachedApplicationQueryClient.GetParams delegates to a real params client.
// Only GetParams is exercised here; the entity-cache GetApplication path is
// covered by application_cache_test.go.
type stubAppParamsClient struct {
	params     *apptypes.Params
	err        error
	paramsHits int
}

func (s *stubAppParamsClient) GetApplication(_ context.Context, _ string) (apptypes.Application, error) {
	return apptypes.Application{}, errors.New("GetApplication not used in this stub")
}

func (s *stubAppParamsClient) GetAllApplications(_ context.Context) ([]apptypes.Application, error) {
	return nil, errors.New("GetAllApplications not used in this stub")
}

func (s *stubAppParamsClient) GetParams(_ context.Context) (*apptypes.Params, error) {
	s.paramsHits++
	if s.err != nil {
		return nil, s.err
	}
	return s.params, nil
}

// TestCachedApplicationQueryClient_GetParams_DelegatesWhenWired proves the HIGH
// bug fix: the meter-facing constructor routes GetParams to a real params client
// instead of the (nil, nil) stub, so the on-chain application MinStake reaches the
// consumer.
func TestCachedApplicationQueryClient_GetParams_DelegatesWhenWired(t *testing.T) {
	ctx := context.Background()
	want := &apptypes.Params{
		MinStake: &cosmostypes.Coin{Denom: "upokt", Amount: cosmosmath.NewInt(1_000_000)},
	}
	stub := &stubAppParamsClient{params: want}

	// nil entity cache is fine: GetParams must not touch it.
	c := NewCachedApplicationQueryClientWithParams(nil, stub)

	got, err := c.GetParams(ctx)
	require.NoError(t, err)
	require.NotNil(t, got, "GetParams must not return the frozen nil stub when a params client is wired")
	require.Equal(t, int64(1_000_000), got.GetMinStake().Amount.Int64(),
		"delegated params must carry the on-chain MinStake")
	require.Equal(t, 1, stub.paramsHits, "GetParams must delegate exactly once")
}

// TestCachedApplicationQueryClient_GetParams_PropagatesError ensures a delegate
// error surfaces to the caller (the meter logs 0 on error, but must not panic).
func TestCachedApplicationQueryClient_GetParams_PropagatesError(t *testing.T) {
	ctx := context.Background()
	stub := &stubAppParamsClient{err: errors.New("chain unreachable")}

	c := NewCachedApplicationQueryClientWithParams(nil, stub)

	got, err := c.GetParams(ctx)
	require.Error(t, err)
	require.Nil(t, got)
	require.Contains(t, err.Error(), "chain unreachable")
}

// TestCachedApplicationQueryClient_GetParams_NilWhenUnwired documents the ring
// variant: it never reads module params, so GetParams stays a no-op (nil, nil).
func TestCachedApplicationQueryClient_GetParams_NilWhenUnwired(t *testing.T) {
	ctx := context.Background()

	c := NewCachedApplicationQueryClient(nil)

	got, err := c.GetParams(ctx)
	require.NoError(t, err)
	require.Nil(t, got, "ring variant must preserve the no-params behavior")
}
