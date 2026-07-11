package redis

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	transportredis "github.com/pokt-network/pocket-relay-miner/transport/redis"
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
  - account: ha:cache:account:{address}
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
  pocket-relay-miner redis cache --type supplier --invalidate --key-file addrs.txt

  # Hot-safe cleanup of ALL regenerable cache keys (safe with live traffic):
  # deletes ha:cache:* (except repopulation locks) plus contaminated
  # ha:supplier:* entries, preserves healthy supplier entries (deleting them
  # would 503 relays until the miner reconcile rewrites them), then publishes
  # a clear-all event so every instance drops its L1 immediately.
  # State keys (sessions, SMST, WAL, registry) are never touched.
  pocket-relay-miner redis cache --type all --invalidate --all [--dry-run|--yes]`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()

			if cacheType == "all" {
				// Hot-safe cleanup of every regenerable cache key. Only the
				// bulk-invalidate form makes sense for the pseudo-type.
				if !invalidate || !all {
					return fmt.Errorf("--type all requires --invalidate --all")
				}
				if key != "" || keyFile != "" || listAll {
					return fmt.Errorf("--type all does not support --key, --key-file, or --list")
				}
				client, err := CreateRedisClient(ctx)
				if err != nil {
					return err
				}
				defer func() { _ = client.Close() }()
				return invalidateAllTypes(ctx, client, dryRun, yes)
			}

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
					return invalidateCache(ctx, client, cacheType, key, yes)
				case all:
					return invalidateAll(ctx, client, cacheType, dryRun, yes)
				case keyFile != "":
					return invalidateFromFile(ctx, client, cacheType, keyFile, dryRun, yes)
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

	cmd.Flags().StringVar(&cacheType, "type", "", "Cache type (application|service|account|supplier|shared_params|session_params|proof_params|all)")
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
	redisKey := buildCacheKey(client.KB(), cacheType, key)

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
func cachePattern(kb *transportredis.KeyBuilder, cacheType string) (pattern string, knownSet string, err error) {
	switch cacheType {
	case "application":
		return kb.CacheKey("application", "*"), kb.CacheKnownKey("applications"), nil
	case "service":
		return kb.CacheKey("service", "*"), kb.CacheKnownKey("services"), nil
	case "account":
		return kb.CacheKey("account", "*"), "", nil
	case "supplier":
		return kb.SupplierStatePattern(), kb.CacheKnownKey("suppliers"), nil
	case "shared_params":
		return kb.ParamsSharedCacheKey(), "", nil
	case "session_params":
		return kb.ParamsSessionKey(), "", nil
	case "proof_params":
		return kb.ParamsProofKey(), "", nil
	default:
		return "", "", fmt.Errorf("unknown cache type: %s", cacheType)
	}
}

// keyFromRedisKey extracts the logical key (address / service id) from a full
// Redis key for a given cache type. Used so pub/sub payloads and known-set
// SREM arguments match what the single-key path uses.
func keyFromRedisKey(kb *transportredis.KeyBuilder, cacheType, redisKey string) string {
	switch cacheType {
	case "application", "service", "account":
		return strings.TrimPrefix(redisKey, kb.CacheKey(cacheType, ""))
	case "supplier":
		return strings.TrimPrefix(redisKey, kb.SupplierStateKey(""))
	default:
		return redisKey
	}
}

func listCacheKeys(ctx context.Context, client *DebugRedisClient, cacheType string) error {
	pattern, knownSetKey, err := cachePattern(client.KB(), cacheType)
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
	keys, err := clusterAwareScanAllKeys(ctx, client, pattern)
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

// confirmProceed prints prompt, reads one stdin line, and returns true only
// on an explicit 'y'. Callers print their own context/warnings first.
func confirmProceed() bool {
	fmt.Printf("Type 'y' to proceed (or use --yes to bypass): ")
	reader := bufio.NewReader(os.Stdin)
	resp, _ := reader.ReadString('\n')
	return strings.TrimSpace(resp) == "y"
}

// supplierWipeWarning is shown before ANY supplier-state invalidation:
// relayers return 503 on a supplier cache miss (fail-open covers Redis
// errors only), so deleting a healthy entry rejects that supplier's relays
// until the miner reconcile rewrites it.
func supplierWipeWarning() {
	fmt.Printf("WARNING: relayers return 503 on supplier cache misses; wiping healthy\n")
	fmt.Printf("supplier entries rejects their relays until the miner reconcile rewrites\n")
	fmt.Printf("them (up to ~60s). For a hot-safe cleanup use --type all instead.\n")
}

// confirmSupplierInvalidation gates every non---all supplier invalidation
// path behind the 503 warning + prompt (unless --yes). Returns false when
// the operator aborted.
func confirmSupplierInvalidation(cacheType string, count int, yes bool) bool {
	if cacheType != "supplier" || yes {
		return true
	}
	fmt.Printf("About to invalidate %d supplier state entr%s.\n", count, map[bool]string{true: "y", false: "ies"}[count == 1])
	supplierWipeWarning()
	if !confirmProceed() {
		fmt.Printf("Aborted. No keys were invalidated.\n")
		return false
	}
	return true
}

// invalidationPayload builds the pub/sub payload for a targeted (single-key)
// invalidation. Each cache's handleInvalidation parses a type-specific field
// (application/account: "address", service: "service_id", supplier:
// "operator_address") — the legacy {"key": ...} payload parsed cleanly but
// matched no field, so remote L1s were never cleared by CLI invalidations.
// The "key" field is kept for backward compatibility with external tooling.
func invalidationPayload(cacheType, key string) string {
	field := ""
	switch cacheType {
	case "application", "account":
		field = "address"
	case "service":
		field = "service_id"
	case "supplier":
		field = "operator_address"
	}
	if field == "" {
		// Params singletons clear their L1 on any payload.
		return fmt.Sprintf(`{"key": %q}`, key)
	}
	return fmt.Sprintf(`{"key": %q, %q: %q}`, key, field, key)
}

// invalidateCache is the single-key invalidate path. Output is preserved
// byte-identical to prior releases for backward compatibility (except the
// supplier confirmation gate, which guards a real 503 window).
func invalidateCache(ctx context.Context, client *DebugRedisClient, cacheType, key string, yes bool) error {
	if !confirmSupplierInvalidation(cacheType, 1, yes) {
		return nil
	}
	redisKey := buildCacheKey(client.KB(), cacheType, key)

	// Delete the key
	if err := client.Del(ctx, redisKey).Err(); err != nil {
		return fmt.Errorf("failed to delete cache key: %w", err)
	}

	fmt.Printf("Invalidated cache entry: %s\n", redisKey)

	// Publish invalidation event
	channel := client.KB().EventChannel(cacheType, "invalidate")
	payload := invalidationPayload(cacheType, key)

	if err := client.Publish(ctx, channel, payload).Err(); err != nil {
		fmt.Printf("Warning: failed to publish invalidation event: %v\n", err)
	} else {
		fmt.Printf("Published invalidation event to channel: %s\n", channel)
	}

	// Best-effort SREM from the known tracking set. Silent on both success and
	// absence so the single-key output stays byte-identical to prior releases.
	if _, knownSet, err := cachePattern(client.KB(), cacheType); err == nil && knownSet != "" {
		_ = client.SRem(ctx, knownSet, key).Err()
	}

	return nil
}

// invalidateOneQuiet performs the same actions as invalidateCache but emits no
// stdout. Used by bulk paths which print progress separately.
func invalidateOneQuiet(ctx context.Context, client *DebugRedisClient, cacheType, key string) error {
	redisKey := buildCacheKey(client.KB(), cacheType, key)
	if err := client.Del(ctx, redisKey).Err(); err != nil {
		return fmt.Errorf("failed to delete cache key %q: %w", redisKey, err)
	}
	channel := client.KB().EventChannel(cacheType, "invalidate")
	payload := invalidationPayload(cacheType, key)
	// Publish is best-effort; a missing subscriber should not fail the bulk op.
	_ = client.Publish(ctx, channel, payload).Err()
	if _, knownSet, err := cachePattern(client.KB(), cacheType); err == nil && knownSet != "" {
		_ = client.SRem(ctx, knownSet, key).Err()
	}
	return nil
}

func invalidateAll(ctx context.Context, client *DebugRedisClient, cacheType string, dryRun, yes bool) error {
	pattern, _, err := cachePattern(client.KB(), cacheType)
	if err != nil {
		return err
	}

	redisKeys, err := clusterAwareScanAllKeys(ctx, client, pattern)
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

	// Suppliers always require confirmation regardless of count: wiping even
	// one healthy supplier entry rejects that supplier's relays (503) until
	// the miner reconcile rewrites it. Other types only prompt above the
	// bulk threshold.
	needsConfirm := total > bulkConfirmThreshold || cacheType == "supplier"
	if needsConfirm && !yes {
		fmt.Printf("About to invalidate %d %s entries (pattern %q).\n", total, cacheType, pattern)
		fmt.Printf("This publishes pub/sub invalidations and removes known-set membership.\n")
		if cacheType == "supplier" {
			supplierWipeWarning()
		}
		if !confirmProceed() {
			fmt.Printf("Aborted. No keys were invalidated.\n")
			return nil
		}
	}

	done := 0
	for _, rk := range redisKeys {
		logicalKey := keyFromRedisKey(client.KB(), cacheType, rk)
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

func invalidateFromFile(ctx context.Context, client *DebugRedisClient, cacheType, path string, dryRun, yes bool) error {
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
	// Empty-file check first: prompting to confirm zero deletions is noise.
	if !dryRun && !confirmSupplierInvalidation(cacheType, total, yes) {
		return nil
	}

	if dryRun {
		fmt.Printf("[dry-run] would invalidate %d %s entries from %s:\n", total, cacheType, path)
		for _, k := range keys {
			fmt.Printf("  - %s\n", buildCacheKey(client.KB(), cacheType, k))
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

func buildCacheKey(kb *transportredis.KeyBuilder, cacheType, key string) string {
	switch cacheType {
	case "application", "service", "account":
		return kb.CacheKey(cacheType, key)
	case "supplier":
		return kb.SupplierStateKey(key)
	case "shared_params":
		return kb.ParamsSharedCacheKey()
	case "session_params":
		return kb.ParamsSessionKey()
	case "proof_params":
		return kb.ParamsProofKey()
	default:
		return key
	}
}
