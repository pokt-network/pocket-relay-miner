package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

func redisDebugMeterCmd() *cobra.Command {
	var (
		sessionID string
		appAddr   string
		serviceID string
		showAll   bool
	)

	cmd := &cobra.Command{
		Use:   "meter",
		Short: "Inspect relay metering data",
		Long: `Inspect relay metering and parameter data in Redis.

Meter data locations:
  - ha:meter:{sessionID} - Session metering data
  - ha:params:shared - Shared on-chain params
  - ha:params:session - Session params
  - ha:app_stake:{appAddress} - App stake info
  - ha:service:{serviceID}:compute_units - Service compute units`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if showAll {
				return showAllMeterKeys(ctx, client)
			}

			if sessionID != "" {
				return inspectSessionMeter(ctx, client, sessionID)
			}

			if appAddr != "" {
				return inspectAppStake(ctx, client, appAddr)
			}

			if serviceID != "" {
				return inspectServiceParams(ctx, client, serviceID)
			}

			return inspectGlobalParams(ctx, client)
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID")
	cmd.Flags().StringVar(&appAddr, "app", "", "Application address")
	cmd.Flags().StringVar(&serviceID, "service", "", "Service ID")
	cmd.Flags().BoolVar(&showAll, "all", false, "Show all meter keys")

	return cmd
}

func inspectSessionMeter(ctx context.Context, client *redisClient, sessionID string) error {
	key := fmt.Sprintf("ha:meter:%s", sessionID)

	data, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("failed to get session meter: %w", err)
	}

	if len(data) == 0 {
		fmt.Printf("No metering data found for session: %s\n", sessionID)
		return nil
	}

	fmt.Printf("Session Metering Data: %s\n\n", sessionID)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "FIELD\tVALUE\n")

	for field, value := range data {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", field, value)
	}

	_ = w.Flush()

	return nil
}

func inspectAppStake(ctx context.Context, client *redisClient, appAddr string) error {
	key := fmt.Sprintf("ha:app_stake:%s", appAddr)

	val, err := client.Get(ctx, key).Result()
	if err == redis.Nil {
		fmt.Printf("No app stake data found for: %s\n", appAddr)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get app stake: %w", err)
	}

	ttl, _ := client.TTL(ctx, key).Result()

	fmt.Printf("App Stake Data\n")
	fmt.Printf("Application: %s\n", appAddr)
	fmt.Printf("TTL: %v\n", ttl)
	fmt.Printf("\nValue:\n%s\n", val)

	return nil
}

func inspectServiceParams(ctx context.Context, client *redisClient, serviceID string) error {
	key := fmt.Sprintf("ha:service:%s:compute_units", serviceID)

	val, err := client.Get(ctx, key).Result()
	if err == redis.Nil {
		fmt.Printf("No service params found for: %s\n", serviceID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to get service params: %w", err)
	}

	ttl, _ := client.TTL(ctx, key).Result()

	fmt.Printf("Service Parameters\n")
	fmt.Printf("Service ID: %s\n", serviceID)
	fmt.Printf("TTL: %v\n", ttl)
	fmt.Printf("\nCompute Units:\n%s\n", val)

	return nil
}

func inspectGlobalParams(ctx context.Context, client *redisClient) error {
	keys := []string{
		"ha:params:shared",
		"ha:params:session",
	}

	fmt.Printf("Global Parameters\n")
	fmt.Printf("=================\n\n")

	for _, key := range keys {
		val, err := client.Get(ctx, key).Result()
		if err == redis.Nil {
			fmt.Printf("%s: Not found\n\n", key)
			continue
		}
		if err != nil {
			fmt.Printf("%s: Error - %v\n\n", key, err)
			continue
		}

		ttl, _ := client.TTL(ctx, key).Result()

		fmt.Printf("%s\n", key)
		fmt.Printf("TTL: %v\n", ttl)
		fmt.Printf("Size: %d bytes\n", len(val))
		fmt.Printf("Value (first 200 chars):\n")
		if len(val) > 200 {
			fmt.Printf("%s...\n\n", val[:200])
		} else {
			fmt.Printf("%s\n\n", val)
		}
	}

	return nil
}

func showAllMeterKeys(ctx context.Context, client *redisClient) error {
	patterns := []string{
		"ha:meter:*",
		"ha:params:*",
		"ha:app_stake:*",
		"ha:service:*",
	}

	fmt.Printf("All Metering Keys\n")
	fmt.Printf("=================\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "KEY\tTYPE\tTTL\n")

	for _, pattern := range patterns {
		var cursor uint64
		for {
			keys, newCursor, err := client.Scan(ctx, cursor, pattern, 100).Result()
			if err != nil {
				return fmt.Errorf("failed to scan keys: %w", err)
			}

			for _, key := range keys {
				keyType, _ := client.Type(ctx, key).Result()
				ttl, _ := client.TTL(ctx, key).Result()
				_, _ = fmt.Fprintf(w, "%s\t%s\t%v\n", key, keyType, ttl)
			}

			cursor = newCursor
			if cursor == 0 {
				break
			}
		}
	}

	_ = w.Flush()

	return nil
}
