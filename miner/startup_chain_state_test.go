//go:build test

package miner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"
)

// countingReaders returns readers that answer with the given values and count
// how many times each was asked.
type countingReaders struct {
	network, params, height atomic.Int32
}

func (c *countingReaders) readers(
	network string, networkErr error,
	params *sharedtypes.Params, paramsErr error,
	height int64, blockTime time.Time, heightErr error,
) startupChainReaders {
	return startupChainReaders{
		network: func(context.Context) (string, error) { c.network.Add(1); return network, networkErr },
		params:  func(context.Context) (*sharedtypes.Params, error) { c.params.Add(1); return params, paramsErr },
		height: func(context.Context) (int64, time.Time, error) {
			c.height.Add(1)
			return height, blockTime, heightErr
		},
	}
}

// TestReadStartupChainState: the miner does not start without a node of its own
// chain, the shared params and a committed height, and says which one failed.
func TestReadStartupChainState(t *testing.T) {
	const chainID = "pocket"
	blockTime := time.Date(2026, 9, 17, 22, 5, 17, 0, time.UTC)
	params := &sharedtypes.Params{NumBlocksPerSession: 10, ClaimWindowOpenOffsetBlocks: 1}
	errNetwork := errors.New("injected: node info unreadable")
	errParams := errors.New("injected: params unreadable")
	errHeight := errors.New("injected: status unreadable")

	tests := []struct {
		name       string
		network    string
		networkErr error
		paramsErr  error
		height     int64
		heightErr  error

		blockTime time.Time
		wantErr   error
		// A node of another chain answers with that chain's params and height,
		// so they must not even be read.
		wantParamsReads, wantHeightReads int32
	}{
		{name: "node on another chain", network: "pocket-beta", height: 100,
			wantErr: errNodeOnAnotherChain},
		{name: "node reports no network", network: "", height: 100,
			wantErr: errNodeOnAnotherChain},
		{name: "network unreadable", networkErr: errNetwork, height: 100,
			wantErr: errNetwork},
		{name: "params unreadable", network: chainID, paramsErr: errParams, height: 100,
			wantErr: errParams, wantParamsReads: 1},
		{name: "height unreadable", network: chainID, heightErr: errHeight,
			wantErr: errHeight, wantParamsReads: 1, wantHeightReads: 1},
		{name: "height zero carries no information", network: chainID, height: 0,
			wantErr: errNoChainHeight, wantParamsReads: 1, wantHeightReads: 1},
		{name: "negative height", network: chainID, height: -3,
			wantErr: errNoChainHeight, wantParamsReads: 1, wantHeightReads: 1},
		{name: "a block with no time anchors nothing", network: chainID, height: 4242, blockTime: time.Time{},
			wantErr: errNoChainBlockTime, wantParamsReads: 1, wantHeightReads: 1},
		{name: "same chain, all read", network: chainID, height: 4242, blockTime: blockTime,
			wantParamsReads: 1, wantHeightReads: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c countingReaders
			var gotParamsIn *sharedtypes.Params
			if tt.paramsErr == nil {
				gotParamsIn = params
			}
			gotParams, gotHeight, gotBlockTime, err := readStartupChainState(context.Background(), chainID,
				c.readers(tt.network, tt.networkErr, gotParamsIn, tt.paramsErr, tt.height, tt.blockTime, tt.heightErr))

			require.Equal(t, int32(1), c.network.Load(), "the network is always read, and read first")
			require.Equal(t, tt.wantParamsReads, c.params.Load(), "params reads")
			require.Equal(t, tt.wantHeightReads, c.height.Load(), "height reads")
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr,
					"LINK startup-reads-block-time: the miner does not start without the chain's block time, and says which read failed")
				require.Nil(t, gotParams)
				require.Zero(t, gotHeight)
				require.Zero(t, gotBlockTime)
				return
			}
			require.NoError(t, err)
			require.Same(t, params, gotParams)
			require.Equal(t, tt.height, gotHeight)
			require.Equal(t, tt.blockTime, gotBlockTime, "LINK startup-reads-block-time: the time of that block comes back with it")
		})
	}
}

// TestReadStartupChainState_BoundsEachRead: each read gets a deadline, so an
// unreachable node fails the start instead of hanging it.
func TestReadStartupChainState_BoundsEachRead(t *testing.T) {
	var networkDeadline, paramsDeadline, heightDeadline bool
	_, _, _, err := readStartupChainState(context.Background(), "pocket", startupChainReaders{
		network: func(ctx context.Context) (string, error) {
			_, networkDeadline = ctx.Deadline()
			return "pocket", nil
		},
		params: func(ctx context.Context) (*sharedtypes.Params, error) {
			_, paramsDeadline = ctx.Deadline()
			return &sharedtypes.Params{}, nil
		},
		height: func(ctx context.Context) (int64, time.Time, error) {
			_, heightDeadline = ctx.Deadline()
			return 1, time.Now(), nil
		},
	})
	require.NoError(t, err)
	require.True(t, networkDeadline, "the network read must carry a deadline")
	require.True(t, paramsDeadline, "the params read must carry a deadline")
	require.True(t, heightDeadline, "the height read must carry a deadline")
}
