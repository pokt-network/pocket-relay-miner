package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
)

func MeterCmd() *cobra.Command {
	var (
		sessionID string
		showAll   bool
	)

	cmd := &cobra.Command{
		Use:   "meter",
		Short: "Inspect relay metering data",
		Long: `Inspect relay metering and parameter data in Redis.

Meter data locations (default namespace shown; all built by the KeyBuilder):
  - ha:meter:{sessionID}:{supplier}:meta     - Per-supplier meter metadata
  - ha:meter:{sessionID}:{supplier}:consumed - Per-supplier consumed stake
  - ha:params:shared - Shared on-chain params
  - ha:params:session - Session params

Metering is per (session, supplier): one session is served by several
suppliers and each meters its own stake, so --session scans for every
supplier that metered it.

An application's staked budget is a field of the meter metadata
(created_with_app_stake), and a service's compute units live in the service
cache -- inspect those with "redis cache --type service --key <id>".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			client, err := CreateRedisClient(ctx)
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

			return inspectGlobalParams(ctx, client)
		},
	}

	cmd.Flags().StringVar(&sessionID, "session", "", "Session ID")
	cmd.Flags().BoolVar(&showAll, "all", false, "Show all meter keys")

	return cmd
}

func inspectSessionMeter(ctx context.Context, client *DebugRedisClient, sessionID string) error {
	// The relayer meters per (session, supplier): one session is served by many
	// suppliers and each has its own cap and consumed counter. Scanning for every
	// supplier's key is what makes this reflect production — reading the bare
	// {base}:{meter}:{session} key addressed nothing any writer produces, so this
	// command reported "no metering data" for sessions that were being metered.
	pattern := client.KB().MeterSessionMetaPattern(sessionID)
	keys, err := clusterAwareScanAllKeys(ctx, client, pattern)
	if err != nil {
		return fmt.Errorf("failed to scan meter keys: %w", err)
	}

	if len(keys) == 0 {
		fmt.Printf("No metering data found for session: %s\n", sessionID)
		return nil
	}

	fmt.Printf("Session Metering Data: %s (%d supplier(s))\n\n", sessionID, len(keys))

	for _, metaKey := range keys {
		data, err := client.HGetAll(ctx, metaKey).Result()
		if err != nil {
			return fmt.Errorf("failed to get meter meta %s: %w", metaKey, err)
		}
		if len(data) == 0 {
			continue
		}

		fmt.Printf("Key: %s\n", metaKey)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintf(w, "FIELD\tVALUE\n")
		for field, value := range data {
			_, _ = fmt.Fprintf(w, "%s\t%s\n", field, value)
		}

		// The consumed counter is a sibling key, not a field of the meta hash;
		// without it the output shows a budget with no spend against it.
		if supplier, ok := supplierFromMeterMetaKey(metaKey); ok {
			consumedKey := client.KB().MeterConsumedKey(sessionID, supplier)
			switch consumed, err := client.Get(ctx, consumedKey).Result(); {
			case err == nil:
				_, _ = fmt.Fprintf(w, "consumed_upokt\t%s\n", consumed)
			case errors.Is(err, redis.Nil):
				_, _ = fmt.Fprintf(w, "consumed_upokt\t<unset>\n")
			default:
				return fmt.Errorf("failed to get consumed key %s: %w", consumedKey, err)
			}
		}

		_ = w.Flush()
		fmt.Println()
	}

	return nil
}

// supplierFromMeterMetaKey extracts the supplier address from a meter meta key
// shaped {base}:{meter}:{session}:{supplier}:meta. It reads positionally from
// the END so that a configured base or meter prefix containing a colon cannot
// shift the field, and a session id never can (the chain's ids are hex).
func supplierFromMeterMetaKey(metaKey string) (string, bool) {
	parts := strings.Split(metaKey, ":")
	if len(parts) < 2 || parts[len(parts)-1] != "meta" {
		return "", false
	}
	supplier := parts[len(parts)-2]
	if supplier == "" {
		return "", false
	}
	return supplier, true
}

func inspectGlobalParams(ctx context.Context, client *DebugRedisClient) error {
	keys := []string{
		client.KB().LegacyParamsKey("shared"),
		client.KB().LegacyParamsKey("session"),
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

func showAllMeterKeys(ctx context.Context, client *DebugRedisClient) error {
	// Only patterns something actually writes. ha:app_stake:* and
	// ha:service:*:compute_units had no writer: an application's stake is a
	// field of the meter metadata (created_with_app_stake) and a service's
	// compute units live in the service cache, so scanning for them listed
	// nothing while implying the data was missing.
	patterns := []string{
		client.KB().MeterSessionKey("*"),
		client.KB().LegacyParamsPattern(),
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
