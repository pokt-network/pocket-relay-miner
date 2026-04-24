package redis

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// bulkConfirmThreshold is the number of keys above which `--all` requires
// an interactive confirmation (can be bypassed with --yes).
const bulkConfirmThreshold = 100

// bulkProgressInterval controls how often bulk progress is printed.
const bulkProgressInterval = 25

func CacheCmd() *cobra.Command {
	var (
		cacheType  string
		key        string
		keyFile    string
		invalidate bool
		listAll    bool
		all        bool
		dryRun     bool
		yes        bool
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
  - ha:cache:known:suppliers

Examples:
  # Inspect a single entry
  pocket-relay-miner redis cache --type supplier --key pokt1abc...

  # List all entries of a type
  pocket-relay-miner redis cache --type supplier --list

  # Invalidate a single entry
  pocket-relay-miner redis cache --type supplier --invalidate --key pokt1abc...

  # Bulk invalidate every entry of a type (SCAN-based, non-blocking)
  pocket-relay-miner redis cache --type supplier --invalidate --all

  # Preview bulk invalidation without deleting
  pocket-relay-miner redis cache --type supplier --invalidate --all --dry-run

  # Skip confirmation prompt on large bulk invalidations
  pocket-relay-miner redis cache --type supplier --invalidate --all --yes

  # Invalidate addresses listed in a file (one per line; '#' comments allowed)
  pocket-relay-miner redis cache --type supplier --invalidate --key-file addrs.txt`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			if invalidate {
				// Count selectors
				sel := 0
				if key != "" {
					sel++
				}
				if all {
					sel++
				}
				if keyFile != "" {
					sel++
				}
				if sel == 0 {
					return fmt.Errorf("--invalidate requires exactly one of --key, --all, or --key-file")
				}
				if sel > 1 {
					return fmt.Errorf("--key, --all, and --key-file are mutually exclusive")
				}
				if dryRun && key != "" {
					return fmt.Errorf("--dry-run is only meaningful with --all or --key-file")
				}
			} else {
				// --dry-run / --all / --key-file / --yes only apply with --invalidate
				if dryRun || all || keyFile != "" || yes {
					return fmt.Errorf("--all, --key-file, --dry-run, and --yes require --invalidate")
				}
			}

			client, err := CreateRedisClient(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()

			if invalidate {
				switch {
				case key != "":
					return invalidateCache(ctx, client, cacheType, key)
				case all:
					return invalidateAll(ctx, client, cacheType, dryRun, yes)
				case keyFile != "":
					return invalidateFromFile(ctx, client, cacheType, keyFile, dryRun)
				}
			}

			if listAll {
				return listCacheKeys(ctx, client, cacheType)
			}

			if key != "" {
				return inspectCacheKey(ctx, client, cacheType, key)
			}

			return fmt.Errorf("specify --key to inspect, --list to list all, or --invalidate with --key/--all/--key-file")
		},
	}

	cmd.Flags().StringVar(&cacheType, "type", "", "Cache type (application|service|supplier|shared_params|session_params|proof_params)")
	cmd.Flags().StringVar(&key, "key", "", "Cache key (address, service ID, etc)")
	cmd.Flags().BoolVar(&invalidate, "invalidate", false, "Invalidate the cache entry")
	cmd.Flags().BoolVar(&all, "all", false, "With --invalidate, invalidate every entry matching the type's prefix")
	cmd.Flags().StringVar(&keyFile, "key-file", "", "With --invalidate, path to file with one key per line ('#' comments allowed)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "With --all or --key-file, print what would be invalidated without deleting")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt for large bulk invalidations")
	cmd.Flags().BoolVar(&listAll, "list", false, "List all cached entries")
	_ = cmd.MarkFlagRequired("type")

	return cmd
}

func inspectCacheKey(ctx context.Context, client *DebugRedisClient, cacheType, key string) error {
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

// cachePattern returns the SCAN pattern and known-set key (if any) for a cache type.
// For singleton types (shared_params, session_params, proof_params) the pattern
// matches the single Redis key.
func cachePattern(cacheType string) (pattern string, knownSet string, err error) {
	switch cacheType {
	case "application":
		return "ha:cache:application:*", "ha:cache:known:applications", nil
	case "service":
		return "ha:cache:service:*", "ha:cache:known:services", nil
	case "supplier":
		return "ha:supplier:*", "ha:cache:known:suppliers", nil
	case "shared_params":
		return "ha:cache:shared_params", "", nil
	case "session_params":
		return "ha:cache:session_params", "", nil
	case "proof_params":
		return "ha:cache:proof_params", "", nil
	default:
		return "", "", fmt.Errorf("unknown cache type: %s", cacheType)
	}
}

// keyFromRedisKey extracts the logical key (address / service id) from a full
// Redis key for a given cache type. Used so pub/sub payloads and known-set
// SREM arguments match what the single-key path uses.
func keyFromRedisKey(cacheType, redisKey string) string {
	switch cacheType {
	case "application":
		return strings.TrimPrefix(redisKey, "ha:cache:application:")
	case "service":
		return strings.TrimPrefix(redisKey, "ha:cache:service:")
	case "supplier":
		return strings.TrimPrefix(redisKey, "ha:supplier:")
	default:
		return redisKey
	}
}

func listCacheKeys(ctx context.Context, client *DebugRedisClient, cacheType string) error {
	pattern, knownSetKey, err := cachePattern(cacheType)
	if err != nil {
		return err
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
	keys, err := scanAllKeys(ctx, client, pattern)
	if err != nil {
		return err
	}

	if len(keys) == 0 {
		fmt.Printf("No %s cache entries found\n", cacheType)
		return nil
	}

	fmt.Printf("Cache entries for type '%s':\n", cacheType)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "KEY\tTTL\tSIZE\n")

	for _, k := range keys {
		ttl, _ := client.TTL(ctx, k).Result()
		size := client.StrLen(ctx, k).Val()
		_, _ = fmt.Fprintf(w, "%s\t%v\t%d bytes\n", k, ttl, size)
	}

	_ = w.Flush()
	fmt.Printf("\nTotal: %d entries\n", len(keys))

	return nil
}

// scanAllKeys uses non-blocking SCAN to enumerate every key matching pattern.
func scanAllKeys(ctx context.Context, client *DebugRedisClient, pattern string) ([]string, error) {
	var cursor uint64
	var keys []string
	for {
		scanKeys, next, err := client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to scan keys with pattern %q: %w", pattern, err)
		}
		keys = append(keys, scanKeys...)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return keys, nil
}

// invalidateCache is the single-key invalidate path. Output is preserved
// byte-identical to prior releases for backward compatibility.
func invalidateCache(ctx context.Context, client *DebugRedisClient, cacheType, key string) error {
	redisKey := buildCacheKey(cacheType, key)

	// Delete the key
	if err := client.Del(ctx, redisKey).Err(); err != nil {
		return fmt.Errorf("failed to delete cache key: %w", err)
	}

	fmt.Printf("Invalidated cache entry: %s\n", redisKey)

	// Publish invalidation event
	channel := fmt.Sprintf("ha:events:cache:%s:invalidate", cacheType)
	payload := fmt.Sprintf(`{"key": "%s"}`, key)

	if err := client.Publish(ctx, channel, payload).Err(); err != nil {
		fmt.Printf("Warning: failed to publish invalidation event: %v\n", err)
	} else {
		fmt.Printf("Published invalidation event to channel: %s\n", channel)
	}

	// Best-effort SREM from the known tracking set. Silent on both success and
	// absence so the single-key output stays byte-identical to prior releases.
	if _, knownSet, err := cachePattern(cacheType); err == nil && knownSet != "" {
		_ = client.SRem(ctx, knownSet, key).Err()
	}

	return nil
}

// invalidateOneQuiet performs the same actions as invalidateCache but emits no
// stdout. Used by bulk paths which print progress separately.
func invalidateOneQuiet(ctx context.Context, client *DebugRedisClient, cacheType, key string) error {
	redisKey := buildCacheKey(cacheType, key)
	if err := client.Del(ctx, redisKey).Err(); err != nil {
		return fmt.Errorf("failed to delete cache key %q: %w", redisKey, err)
	}
	channel := fmt.Sprintf("ha:events:cache:%s:invalidate", cacheType)
	payload := fmt.Sprintf(`{"key": "%s"}`, key)
	// Publish is best-effort; a missing subscriber should not fail the bulk op.
	_ = client.Publish(ctx, channel, payload).Err()
	if _, knownSet, err := cachePattern(cacheType); err == nil && knownSet != "" {
		_ = client.SRem(ctx, knownSet, key).Err()
	}
	return nil
}

func invalidateAll(ctx context.Context, client *DebugRedisClient, cacheType string, dryRun, yes bool) error {
	pattern, _, err := cachePattern(cacheType)
	if err != nil {
		return err
	}

	redisKeys, err := scanAllKeys(ctx, client, pattern)
	if err != nil {
		return err
	}

	total := len(redisKeys)

	if total == 0 {
		fmt.Printf("No %s cache entries found (pattern %q)\n", cacheType, pattern)
		fmt.Printf("invalidated 0 entries total\n")
		return nil
	}

	if dryRun {
		fmt.Printf("[dry-run] would invalidate %d %s entries matching %q:\n", total, cacheType, pattern)
		for _, k := range redisKeys {
			fmt.Printf("  - %s\n", k)
		}
		fmt.Printf("[dry-run] no keys were deleted\n")
		return nil
	}

	if total > bulkConfirmThreshold && !yes {
		fmt.Printf("About to invalidate %d %s entries (pattern %q).\n", total, cacheType, pattern)
		fmt.Printf("This publishes pub/sub invalidations and removes known-set membership.\n")
		fmt.Printf("Type 'y' to proceed (or use --yes to bypass): ")
		reader := bufio.NewReader(os.Stdin)
		resp, _ := reader.ReadString('\n')
		if strings.TrimSpace(resp) != "y" {
			fmt.Printf("Aborted. No keys were invalidated.\n")
			return nil
		}
	}

	done := 0
	for _, rk := range redisKeys {
		logicalKey := keyFromRedisKey(cacheType, rk)
		if err := invalidateOneQuiet(ctx, client, cacheType, logicalKey); err != nil {
			return fmt.Errorf("bulk invalidate failed at key %q (completed %d/%d): %w", rk, done, total, err)
		}
		done++
		if done%bulkProgressInterval == 0 {
			fmt.Printf("invalidated %d/%d...\n", done, total)
		}
	}
	fmt.Printf("invalidated %d entries total\n", done)
	return nil
}

func invalidateFromFile(ctx context.Context, client *DebugRedisClient, cacheType, path string, dryRun bool) error {
	keys, err := readKeyFile(path)
	if err != nil {
		return err
	}

	total := len(keys)
	if total == 0 {
		fmt.Printf("key-file %q contained no keys (blank lines and '#' comments are ignored)\n", path)
		fmt.Printf("invalidated 0 entries total\n")
		return nil
	}

	if dryRun {
		fmt.Printf("[dry-run] would invalidate %d %s entries from %s:\n", total, cacheType, path)
		for _, k := range keys {
			fmt.Printf("  - %s\n", buildCacheKey(cacheType, k))
		}
		fmt.Printf("[dry-run] no keys were deleted\n")
		return nil
	}

	done := 0
	for _, k := range keys {
		if err := invalidateOneQuiet(ctx, client, cacheType, k); err != nil {
			return fmt.Errorf("bulk invalidate from file failed at key %q (completed %d/%d): %w", k, done, total, err)
		}
		done++
		if done%bulkProgressInterval == 0 {
			fmt.Printf("invalidated %d/%d...\n", done, total)
		}
	}
	fmt.Printf("invalidated %d entries total\n", done)
	return nil
}

func readKeyFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open key-file %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var keys []string
	scanner := bufio.NewScanner(f)
	// Allow long lines (cosmos-style addresses + future bech32 variants).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read key-file %q: %w", path, err)
	}
	return keys, nil
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
