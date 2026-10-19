//go:build test

package query

import (
	"context"
	"net"
	"testing"
	"time"

	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// appliedPlans answers the upgrade AppliedPlan query from a fixed table.
type appliedPlans struct {
	upgradev1beta1.UnimplementedQueryServer
	heights map[string]int64
	err     error
}

func (s *appliedPlans) AppliedPlan(_ context.Context, req *upgradev1beta1.QueryAppliedPlanRequest) (*upgradev1beta1.QueryAppliedPlanResponse, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &upgradev1beta1.QueryAppliedPlanResponse{Height: s.heights[req.Name]}, nil
}

func newUpgradeTestClients(t *testing.T, answers *appliedPlans) *Clients {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := grpc.NewServer()
	upgradev1beta1.RegisterQueryServer(server, answers)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		<-served
	})

	qc, err := NewQueryClients(logging.NewLoggerFromConfig(logging.DefaultConfig()), ClientConfig{
		GRPCEndpoint: listener.Addr().String(),
		QueryTimeout: 5 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = qc.Close() })
	return qc
}

func TestAppliedPlanHeight_ReadsTheNamedPlan(t *testing.T) {
	qc := newUpgradeTestClients(t, &appliedPlans{heights: map[string]int64{"v0.1.36": 1234}})
	ctx := context.Background()

	height, err := qc.Upgrade().AppliedPlanHeight(ctx, "v0.1.36")
	require.NoError(t, err)
	require.Equal(t, int64(1234), height)

	height, err = qc.Upgrade().AppliedPlanHeight(ctx, "v0.1.37")
	require.NoError(t, err)
	require.Zero(t, height, "a plan not applied answers 0")
}

func TestAppliedPlanHeight_ReturnsQueryFailures(t *testing.T) {
	qc := newUpgradeTestClients(t, &appliedPlans{err: status.Error(codes.Unavailable, "node unavailable")})

	height, err := qc.Upgrade().AppliedPlanHeight(context.Background(), "v0.1.36")
	require.Error(t, err)
	require.Zero(t, height)
	require.Equal(t, codes.Unavailable, status.Code(err))
}
