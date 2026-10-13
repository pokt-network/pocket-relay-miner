//go:build test

package query

import (
	"context"
	"strconv"
	"testing"
	"time"

	grpctypes "github.com/cosmos/cosmos-sdk/types/grpc"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// A claim the node does not have comes back as NotFound with the height the
// node answered from, read from the header cosmos stamps on every answer: the
// pre-proof guard takes it as the chain's verdict only from a height where no
// claim can still be on its way. Without the header the height is 0.
func TestGetClaim_ANotFoundCarriesTheHeightTheNodeAnsweredFrom(t *testing.T) {
	_, address, cleanup, mock := setupMockQueryServer(t)
	defer cleanup()
	stamp := int64(0)
	mock.getClaimFunc = func(ctx context.Context, _ *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
		if stamp > 0 {
			_ = grpc.SetHeader(ctx, metadata.Pairs(grpctypes.GRPCBlockHeightHeader, strconv.FormatInt(stamp, 10)))
		}
		return nil, status.Error(codes.NotFound, "claim not found")
	}
	qc, err := NewQueryClients(logging.NewLoggerFromConfig(logging.DefaultConfig()), ClientConfig{GRPCEndpoint: address, QueryTimeout: 5 * time.Second})
	require.NoError(t, err)
	defer func() { _ = qc.Close() }()

	stamp = 1234
	_, err = qc.Proof().GetClaim(context.Background(), "pokt1supplier", "session-a")
	require.True(t, IsEntityNotFound(err), "still a NotFound to every caller")
	require.Equal(t, int64(1234), AnsweredHeight(err))

	stamp = 0
	_, err = qc.Proof().GetClaim(context.Background(), "pokt1supplier", "session-b")
	require.True(t, IsEntityNotFound(err))
	require.Zero(t, AnsweredHeight(err), "no header: the height is unknown")
}
