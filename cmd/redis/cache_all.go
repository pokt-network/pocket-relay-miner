package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/cache"
)

// `redis cache --type all --invalidate --all` — hot-safe cleanup of every
// regenerable cache key, executable while relayers and miners serve traffic.
//
// Safety invariants (see docs/superpowers/specs/2026-07-10-cache-cleanup-all-design.md):
//  1. State keys ({base}:miner:*, {base}:smst:*, {base}:relays:*,
//     {base}:suppliers:*, {base}:tx:*) are never touched.
//  2. Healthy supplier entries are never deleted: the relayer returns 503 on
//     a supplier cache miss (fail-open covers Redis errors only), so wiping
//     a healthy entry rejects that supplier's relays until the miner
//     reconcile rewrites it. Only contaminated entries (staked+active with
//     empty services) are deleted — those are already treated as misses by
//     the read guard, and the deletion re-checks contamination atomically
//     under WATCH so a concurrently-healed entry is preserved.
//  3. Repopulation locks ({cachePrefix}:lock:*) are preserved: deleting a
//     held lock breaks the mutual exclusion that bounds the post-cleanup L3
//     refill. Locks self-expire via TTL.
//  4. The `{}` clear-all events publish only AFTER all deletions complete;
//     publishing earlier would let a subscriber's L1 repopulate from a
//     half-deleted L2.
const (
	// delBatchSize bounds each DEL pipeline so a large cleanup never sends
	// one giant burst. Deletes are issued as single-key DELs inside a
	// pipeline (not one multi-key DEL) so Redis Cluster deployments don't
	// fail with CROSSSLOT.
	delBatchSize = 500
)

// clearAllChannelTypes are the cache types whose SubscribeToInvalidations
// consumer understands the `{}` clear-all payload. session_params has no
// subscriber (L2 deleted via the cache pattern; L1 ages out by TTL);
// supplier_params subscribes on a nonstandard channel handled separately in
// cleanupScope.channels.
var clearAllChannelTypes = []string{
	"application",
	"service",
	"supplier",
	"account",
	"shared_params",
	"proof_params",
}

// cleanupScope holds the namespace-derived patterns and channels for one
// cleanup run. Everything is built from the client's KeyBuilder so a custom
// Redis namespace (--config with base_prefix != "ha") scopes both the
// deletions and the notifications to the right deployment — a hardcoded
// "ha:" would silently no-op on a custom namespace, or worse, clean a
// neighboring deployment sharing the same Redis.
type cleanupScope struct {
	cachePattern    string   // {base}:{cache}:*
	lockPrefix      string   // {base}:{cache}:lock:
	cacheKeyPrefix  string   // {base}:{cache}:
	supplierPattern string   // {base}:{supplier}:*
	channels        []string // clear-all destinations, in publish order
}

func newCleanupScope(client *DebugRedisClient) cleanupScope {
	kb := client.KB()
	cachePrefix := kb.CachePrefix()
	channels := make([]string, 0, len(clearAllChannelTypes)+1)
	for _, t := range clearAllChannelTypes {
		channels = append(channels, kb.EventChannel(t, "invalidate"))
	}
	// RedisSupplierParamCache subscribes on {EventsCachePrefix}:invalidate:supplier_params:
	// the miner wires its PubSubPrefix to KB().EventsCachePrefix() (see
	// miner/leader_controller.go), not to the EventChannel scheme. Its L1 has
	// no TTL — without this publish, running instances would serve the
	// pre-cleanup supplier params until restart. The handler clears on any
	// payload.
	channels = append(channels, kb.SupplierParamsInvalidateChannel())

	// Deliberately NOT notified: the relayer's height-keyed
	// RedisSharedParamCache (cache/shared_params.go) subscribes on
	// {base}:{events}:invalidate:params but only understands numeric
	// per-height payloads — there is no clear-all it can parse. Safe to skip:
	// its L2 keys ({cachePrefix}:params:shared:{height}) are height-immutable
	// and regenerable via L3, and its L1 TTL is one block.

	return cleanupScope{
		cachePattern:    cachePrefix + ":*",
		lockPrefix:      cachePrefix + ":lock:",
		cacheKeyPrefix:  cachePrefix + ":",
		supplierPattern: kb.SupplierStatePattern(),
		channels:        channels,
	}
}

// cleanupPlan is the classified result of scanning the cleanup scope.
type cleanupPlan struct {
	// cacheKeys is every {base}:{cache}:* key to DEL (locks excluded).
	cacheKeys []string
	// supplierCandidates are supplier keys that looked contaminated at scan
	// time. They are re-checked atomically at delete time (see
	// deleteIfStillContaminated) — this list is a candidate set, not a
	// commitment.
	supplierCandidates []string
	// deleteCounts breaks the plan down by display group for reporting.
	deleteCounts map[string]int
	// locksPreserved counts lock keys left untouched.
	locksPreserved int
	// suppliersHealthy counts healthy supplier entries left untouched.
	suppliersHealthy int
	// supplierReadErrors counts supplier keys skipped because their value
	// could not be read or parsed; they are preserved (fail-safe: never
	// delete what we cannot classify).
	supplierReadErrors int
}

func (p *cleanupPlan) totalPlanned() int {
	return len(p.cacheKeys) + len(p.supplierCandidates)
}

// cacheKeyGroup maps a full cache Redis key to a display group for the
// per-type breakdown.
func cacheKeyGroup(scope cleanupScope, redisKey string) string {
	rest := strings.TrimPrefix(redisKey, scope.cacheKeyPrefix)
	if idx := strings.IndexByte(rest, ':'); idx > 0 {
		return rest[:idx]
	}
	// Singletons: {base}:{cache}:shared_params, :session_params, ...
	return rest
}

// buildCleanupPlan scans the cleanup scope and classifies every key.
func buildCleanupPlan(ctx context.Context, client *DebugRedisClient, scope cleanupScope) (*cleanupPlan, error) {
	plan := &cleanupPlan{deleteCounts: make(map[string]int)}

	cacheKeys, err := clusterAwareScanAllKeys(ctx, client, scope.cachePattern)
	if err != nil {
		return nil, err
	}
	for _, k := range cacheKeys {
		if strings.HasPrefix(k, scope.lockPrefix) {
			plan.locksPreserved++
			continue
		}
		plan.cacheKeys = append(plan.cacheKeys, k)
		plan.deleteCounts[cacheKeyGroup(scope, k)]++
	}

	supplierKeys, err := clusterAwareScanAllKeys(ctx, client, scope.supplierPattern)
	if err != nil {
		return nil, err
	}
	for _, k := range supplierKeys {
		data, getErr := client.Get(ctx, k).Bytes()
		if getErr != nil {
			plan.supplierReadErrors++
			continue
		}
		var state cache.SupplierState
		if jsonErr := json.Unmarshal(data, &state); jsonErr != nil {
			plan.supplierReadErrors++
			continue
		}
		if state.IsContaminated() {
			plan.supplierCandidates = append(plan.supplierCandidates, k)
			continue
		}
		plan.suppliersHealthy++
	}
	plan.deleteCounts["supplier (contaminated)"] = len(plan.supplierCandidates)

	return plan, nil
}

// printCleanupBreakdown prints the per-type breakdown of a cleanup plan.
func printCleanupBreakdown(scope cleanupScope, plan *cleanupPlan) {
	groups := make([]string, 0, len(plan.deleteCounts))
	for g := range plan.deleteCounts {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		fmt.Printf("  %-28s %d keys\n", g, plan.deleteCounts[g])
	}
	fmt.Printf("Preserved: %d repopulation locks (%s*), %d healthy supplier entries",
		plan.locksPreserved, scope.lockPrefix, plan.suppliersHealthy)
	if plan.supplierReadErrors > 0 {
		fmt.Printf(", %d unreadable supplier entries (fail-safe)", plan.supplierReadErrors)
	}
	fmt.Printf("\n")
}

// errSupplierPreserved signals the WATCH callback found the entry healed (or
// gone) and the delete must not happen. Never returned to the caller.
var errSupplierPreserved = errors.New("supplier entry no longer contaminated")

// deleteIfStillContaminated re-checks contamination under WATCH and deletes
// the key only if it is STILL contaminated, atomically. This closes the
// classify-then-delete TOCTOU: the miner reconcile rewrites every supplier
// key on its cadence, and an entry that was contaminated at scan time (e.g.
// written while BlockClient height was 0 at miner boot) may legitimately be
// healthy by delete time — deleting it then would 503 that supplier's
// relays until the next reconcile pass. If the value changes between GET and
// EXEC, the transaction aborts (TxFailedErr) and the entry is preserved:
// losing a delete is safe (the next cleanup catches it), deleting a healthy
// entry is not.
//
// Returns true if the key was deleted.
func deleteIfStillContaminated(ctx context.Context, client *DebugRedisClient, key string) (bool, error) {
	err := client.Watch(ctx, func(tx *goredis.Tx) error {
		data, getErr := tx.Get(ctx, key).Bytes()
		if getErr != nil {
			// Gone or unreadable — nothing to delete / fail-safe preserve.
			return errSupplierPreserved
		}
		var state cache.SupplierState
		if jsonErr := json.Unmarshal(data, &state); jsonErr != nil {
			return errSupplierPreserved
		}
		if !state.IsContaminated() {
			return errSupplierPreserved
		}
		_, txErr := tx.TxPipelined(ctx, func(pipe goredis.Pipeliner) error {
			pipe.Del(ctx, key)
			return nil
		})
		return txErr
	}, key)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, errSupplierPreserved), errors.Is(err, goredis.TxFailedErr):
		return false, nil
	default:
		return false, err
	}
}

// publishClearAll publishes the `{}` clear-all payload to every channel in
// the scope and returns how many publishes succeeded. Publish failures warn
// and continue: the L2 is already consistent, and subscriber L1s age out via
// TTL. Declared as a package variable so tests can wrap it to assert it runs
// only after every deletion completed (invariant 4).
var publishClearAll = func(ctx context.Context, client *DebugRedisClient, scope cleanupScope) int {
	published := 0
	for _, channel := range scope.channels {
		if err := client.Publish(ctx, channel, "{}").Err(); err != nil {
			fmt.Printf("Warning: failed to publish clear-all to %s: %v\n", channel, err)
			continue
		}
		published++
	}
	return published
}

// invalidateAllTypes implements `redis cache --type all --invalidate --all`.
func invalidateAllTypes(ctx context.Context, client *DebugRedisClient, dryRun, yes bool) error {
	scope := newCleanupScope(client)

	plan, err := buildCleanupPlan(ctx, client, scope)
	if err != nil {
		return fmt.Errorf("failed to build cleanup plan: %w", err)
	}

	total := plan.totalPlanned()

	if dryRun {
		fmt.Printf("[dry-run] would invalidate %d cache keys:\n", total)
		printCleanupBreakdown(scope, plan)
		fmt.Printf("[dry-run] no keys were deleted\n")
		return nil
	}

	if total == 0 {
		fmt.Printf("No regenerable cache keys found\n")
		printCleanupBreakdown(scope, plan)
		fmt.Printf("invalidated 0 entries total\n")
		// Still publish the clear-all: a re-run after a crash between the
		// delete phase and the publish phase finds zero keys but must still
		// tell subscribers to drop their (now orphaned) L1 entries.
		published := publishClearAll(ctx, client, scope)
		fmt.Printf("Published clear-all to %d/%d invalidation channels\n", published, len(scope.channels))
		return nil
	}

	if !yes {
		fmt.Printf("About to invalidate %d regenerable cache keys:\n", total)
		printCleanupBreakdown(scope, plan)
		fmt.Printf("State keys (sessions, SMST, WAL, registry) are never touched.\n")
		fmt.Printf("First read per entity after cleanup pays an L3 chain query (~100ms) until repopulated.\n")
		if !confirmProceed() {
			fmt.Printf("Aborted. No keys were invalidated.\n")
			return nil
		}
	}

	// Deletions first, clear-all publish last (invariant 4).
	//
	// Cache keys: single-key DELs inside a pipeline per batch — one round
	// trip per batch on standalone Redis, and no CROSSSLOT on Redis Cluster
	// (a single multi-key DEL spanning hash slots would be rejected there).
	deleted := 0
	for start := 0; start < len(plan.cacheKeys); start += delBatchSize {
		end := start + delBatchSize
		if end > len(plan.cacheKeys) {
			end = len(plan.cacheKeys)
		}
		batch := plan.cacheKeys[start:end]
		if _, err := client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, k := range batch {
				pipe.Del(ctx, k)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("bulk delete failed at batch %d-%d (deleted %d/%d): %w",
				start, end, deleted, total, err)
		}
		deleted += len(batch)
		if len(plan.cacheKeys) > delBatchSize {
			fmt.Printf("deleted %d/%d...\n", deleted, total)
		}
	}

	// Supplier candidates: atomic re-check + delete per key (TOCTOU guard).
	suppliersDeleted := 0
	suppliersHealed := 0
	for _, k := range plan.supplierCandidates {
		ok, delErr := deleteIfStillContaminated(ctx, client, k)
		if delErr != nil {
			return fmt.Errorf("conditional supplier delete failed at %q (deleted %d/%d): %w",
				k, deleted, total, delErr)
		}
		if ok {
			deleted++
			suppliersDeleted++
		} else {
			suppliersHealed++
		}
	}
	if suppliersHealed > 0 {
		// Keep the report honest: these were counted in the plan but were
		// healed (or rewritten) between scan and delete, so they survive.
		plan.deleteCounts["supplier (contaminated)"] = suppliersDeleted
		plan.suppliersHealthy += suppliersHealed
		fmt.Printf("%d supplier entries healed between scan and delete; preserved\n", suppliersHealed)
	}

	published := publishClearAll(ctx, client, scope)

	fmt.Printf("invalidated %d entries total\n", deleted)
	printCleanupBreakdown(scope, plan)
	fmt.Printf("Published clear-all to %d/%d invalidation channels\n", published, len(scope.channels))
	return nil
}
