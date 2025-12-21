package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func LeaderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "leader",
		Short: "Check leader election status",
		Long: `Check the current leader election status.

Leader election uses:
  - Key: ha:miner:global_leader
  - Value: Instance ID of current leader
  - TTL: 30 seconds

The leader must renew every 10 seconds to maintain leadership.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			return checkLeader(ctx, client)
		},
	}

	return cmd
}

func checkLeader(ctx context.Context, client *DebugRedisClient) error {
	key := "ha:miner:global_leader"

	// Check if leader exists
	exists, err := client.Exists(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to check leader existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("No active leader\n")
		fmt.Printf("Leader key: %s\n", key)
		return nil
	}

	// Get leader instance ID
	instanceID, err := client.Get(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get leader instance: %w", err)
	}

	// Get TTL
	ttl, err := client.TTL(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get leader TTL: %w", err)
	}

	fmt.Printf("Leader Election Status\n")
	fmt.Printf("======================\n\n")
	fmt.Printf("Current Leader: %s\n", instanceID)
	fmt.Printf("TTL Remaining: %v\n", ttl)
	fmt.Printf("Leader Key: %s\n\n", key)

	// Calculate health
	if ttl > 20*time.Second {
		fmt.Printf("Status: Healthy (recently renewed)\n")
	} else if ttl > 10*time.Second {
		fmt.Printf("Status: OK\n")
	} else if ttl > 0 {
		fmt.Printf("Status: WARNING - TTL low, leader may be struggling\n")
	} else {
		fmt.Printf("Status: CRITICAL - Negative TTL\n")
	}

	return nil
}
