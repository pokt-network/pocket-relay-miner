package cmd

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func redisDebugCacheCmd() *cobra.Command {
	var (
		cacheType  string
		key        string
		invalidate bool
		listAll    bool
	)

	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect cache entries",
		Long: `Inspect and manage cache entries in Redis.

Cache types:
  - application: ha:cache:application:{address}
  - service: ha:cache:service:{serviceID}
  - supplier: ha:supplier:{address}
  - shared_params: ha:cache:shared_params
  - session_params: ha:cache:session_params
  - proof_params: ha:cache:proof_params

Cache tracking sets:
  - ha:cache:known:applications
  - ha:cache:known:services
  - ha:cache:known:suppliers`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := createRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if invalidate && key != "" {
				return invalidateCache(ctx, client, cacheType, key)
			}

			if listAll {
				return listCacheKeys(ctx, client, cacheType)
			}

			if key != "" {
				return inspectCacheKey(ctx, client, cacheType, key)
			}

			return fmt.Errorf("specify --key to inspect, --list to list all, or --invalidate with --key")
		},
	}

	cmd.Flags().StringVar(&cacheType, "type", "", "Cache type (application|service|supplier|shared_params|session_params|proof_params)")
	cmd.Flags().StringVar(&key, "key", "", "Cache key (address, service ID, etc)")
	cmd.Flags().BoolVar(&invalidate, "invalidate", false, "Invalidate the cache entry")
	cmd.Flags().BoolVar(&listAll, "list", false, "List all cached entries")
	_ = cmd.MarkFlagRequired("type")

	return cmd
}

func inspectCacheKey(ctx context.Context, client *redisClient, cacheType, key string) error {
	redisKey := buildCacheKey(cacheType, key)

	exists, err := client.Exists(ctx, redisKey).Result()
	if err != nil {
		return fmt.Errorf("failed to check cache existence: %w", err)
	}

	if exists == 0 {
		fmt.Printf("Cache entry not found: %s\n", redisKey)
		return nil
	}

	// Get value
	val, err := client.Get(ctx, redisKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get cache value: %w", err)
	}

	// Get TTL
	ttl, err := client.TTL(ctx, redisKey).Result()
	if err != nil {
		return fmt.Errorf("failed to get TTL: %w", err)
	}

	fmt.Printf("Cache Entry: %s\n", redisKey)
	fmt.Printf("TTL: %v\n", ttl)
	fmt.Printf("Size: %d bytes\n", len(val))
	fmt.Printf("\nValue (first 500 chars):\n")
	if len(val) > 500 {
		fmt.Printf("%s...\n", val[:500])
	} else {
		fmt.Printf("%s\n", val)
	}

	return nil
}

func listCacheKeys(ctx context.Context, client *redisClient, cacheType string) error {
	var knownSetKey string
	var pattern string

	switch cacheType {
	case "application":
		knownSetKey = "ha:cache:known:applications"
		pattern = "ha:cache:application:*"
	case "service":
		knownSetKey = "ha:cache:known:services"
		pattern = "ha:cache:service:*"
	case "supplier":
		knownSetKey = "ha:cache:known:suppliers"
		pattern = "ha:supplier:*"
	case "shared_params":
		pattern = "ha:cache:shared_params"
	case "session_params":
		pattern = "ha:cache:session_params"
	case "proof_params":
		pattern = "ha:cache:proof_params"
	default:
		return fmt.Errorf("unknown cache type: %s", cacheType)
	}

	// Try known set first
	if knownSetKey != "" {
		members, err := client.SMembers(ctx, knownSetKey).Result()
		if err == nil && len(members) > 0 {
			fmt.Printf("Known %s entries (from tracking set):\n", cacheType)
			for _, member := range members {
				fmt.Printf("  - %s\n", member)
			}
			fmt.Printf("\nTotal: %d entries\n", len(members))
			return nil
		}
	}

	// Fall back to SCAN
	var cursor uint64
	var keys []string

	for {
		var scanKeys []string
		var err error
		scanKeys, cursor, err = client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return fmt.Errorf("failed to scan keys: %w", err)
		}

		keys = append(keys, scanKeys...)

		if cursor == 0 {
			break
		}
	}

	if len(keys) == 0 {
		fmt.Printf("No %s cache entries found\n", cacheType)
		return nil
	}

	fmt.Printf("Cache entries for type '%s':\n", cacheType)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "KEY\tTTL\tSIZE\n")

	for _, key := range keys {
		ttl, _ := client.TTL(ctx, key).Result()
		size := client.StrLen(ctx, key).Val()
		_, _ = fmt.Fprintf(w, "%s\t%v\t%d bytes\n", key, ttl, size)
	}

	_ = w.Flush()
	fmt.Printf("\nTotal: %d entries\n", len(keys))

	return nil
}

func invalidateCache(ctx context.Context, client *redisClient, cacheType, key string) error {
	redisKey := buildCacheKey(cacheType, key)

	// Delete the key
	err := client.Del(ctx, redisKey).Err()
	if err != nil {
		return fmt.Errorf("failed to delete cache key: %w", err)
	}

	fmt.Printf("Invalidated cache entry: %s\n", redisKey)

	// Publish invalidation event
	channel := fmt.Sprintf("ha:events:cache:%s:invalidate", cacheType)
	payload := fmt.Sprintf(`{"key": "%s"}`, key)

	err = client.Publish(ctx, channel, payload).Err()
	if err != nil {
		fmt.Printf("Warning: failed to publish invalidation event: %v\n", err)
	} else {
		fmt.Printf("Published invalidation event to channel: %s\n", channel)
	}

	return nil
}

func buildCacheKey(cacheType, key string) string {
	switch cacheType {
	case "application":
		return fmt.Sprintf("ha:cache:application:%s", key)
	case "service":
		return fmt.Sprintf("ha:cache:service:%s", key)
	case "supplier":
		return fmt.Sprintf("ha:supplier:%s", key)
	case "shared_params":
		return "ha:cache:shared_params"
	case "session_params":
		return "ha:cache:session_params"
	case "proof_params":
		return "ha:cache:proof_params"
	default:
		return key
	}
}
