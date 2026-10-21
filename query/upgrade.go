package query

import (
	"context"
	"fmt"
	"time"

	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
	"google.golang.org/grpc"
)

// UpgradeQueryClient reads the chain's upgrade module.
type UpgradeQueryClient interface {
	// AppliedPlanHeight returns the height at which the named upgrade plan was
	// applied, or 0 while it has not been.
	AppliedPlanHeight(ctx context.Context, name string) (int64, error)
}

type upgradeQueryClient struct {
	queryClient  upgradev1beta1.QueryClient
	queryTimeout time.Duration
}

func newUpgradeQueryClient(conn grpc.ClientConnInterface, timeout time.Duration) *upgradeQueryClient {
	return &upgradeQueryClient{
		queryClient:  upgradev1beta1.NewQueryClient(conn),
		queryTimeout: timeout,
	}
}

func (c *upgradeQueryClient) AppliedPlanHeight(ctx context.Context, name string) (int64, error) {
	queryCtx, cancel := context.WithTimeout(ctx, c.queryTimeout)
	defer cancel()

	res, err := c.queryClient.AppliedPlan(queryCtx, &upgradev1beta1.QueryAppliedPlanRequest{Name: name})
	if err != nil {
		return 0, fmt.Errorf("failed to query applied plan %q: %w", name, err)
	}
	return res.GetHeight(), nil
}
