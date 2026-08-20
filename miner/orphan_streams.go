package miner

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ScanFunc lists every key matching a glob.
//
// It is a parameter rather than a method call because the two callers must scan
// differently: the CLI uses a cluster-aware scan that walks every shard, while
// the miner uses the plain SCAN it uses everywhere else. Only the CLASSIFICATION
// below is shared, and it is shared precisely because it is the part that would
// diverge silently -- two definitions of "known" drifting apart would show up as
// a delete that should not have happened, long after the change that caused it.
type ScanFunc func(ctx context.Context, pattern string) ([]string, error)

// KnownSupplierAddresses returns every supplier address this deployment has a
// record of.
//
// It is the UNION of two sources, and both halves are load-bearing:
//
//   - the registry index, which a supplier joins when its pipeline starts and
//     leaves when it is torn down cleanly. A miner that crashed leaves its
//     entries behind, so the index alone over-states.
//   - the supplier cache, which a torn-down supplier keeps on purpose: that
//     entry is the chain's answer cached, and the relayer needs it to keep
//     refusing relays for a supplier whose services have been deactivated.
//     Reading the index alone would call such a supplier unknown while its
//     claims are still being settled.
//
// Taking the union means only a supplier that NOTHING claims is treated as gone.
func KnownSupplierAddresses(
	ctx context.Context,
	client *redisutil.Client,
	scan ScanFunc,
) (map[string]struct{}, error) {
	known := make(map[string]struct{})

	indexed, err := client.SMembers(ctx, client.KB().SuppliersRegistryIndexKey()).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("failed to read the supplier registry index: %w", err)
	}
	for _, addr := range indexed {
		known[addr] = struct{}{}
	}

	cachePrefix := client.KB().SupplierKeyPrefix() + ":"
	cached, err := scan(ctx, cachePrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("failed to scan supplier cache keys: %w", err)
	}
	for _, key := range cached {
		if addr := strings.TrimPrefix(key, cachePrefix); addr != key && addr != "" {
			known[addr] = struct{}{}
		}
	}

	return known, nil
}

// OrphanStreamAddresses returns the supplier addresses whose relay stream still
// exists although this deployment no longer has any record of the supplier.
//
// A relay stream is a permanent lane: it carries no expiry, because a clock is
// the wrong instrument for ending a supplier's life. The cost of that choice is
// that a supplier decommissioned for good leaves its lane behind, and this is
// what makes those lanes visible. Nothing here deletes: removing a stream key
// removes its consumer group and that group's pending entries with it, and the
// consumer recreates the group empty on its next connect, so the loss would be
// both unrecoverable and unnoticeable.
func OrphanStreamAddresses(
	ctx context.Context,
	client *redisutil.Client,
	scan ScanFunc,
) ([]string, error) {
	streams, err := scan(ctx, client.KB().StreamPattern())
	if err != nil {
		return nil, fmt.Errorf("failed to scan relay streams: %w", err)
	}
	if len(streams) == 0 {
		return nil, nil
	}

	known, err := KnownSupplierAddresses(ctx, client, scan)
	if err != nil {
		return nil, err
	}

	streamPrefix := client.KB().StreamPrefix() + ":"
	var orphans []string
	for _, key := range streams {
		addr := strings.TrimPrefix(key, streamPrefix)
		if addr == key || addr == "" {
			continue // not a supplier stream under this namespace
		}
		if _, ok := known[addr]; ok {
			continue
		}
		orphans = append(orphans, addr)
	}
	return orphans, nil
}

// ScanKeys is the miner's plain SCAN, matching what the rest of the package
// uses. It is NOT cluster-aware; on a Redis Cluster it reports only the shard
// the client happens to talk to, which under-counts rather than over-counts --
// the safe direction for a signal whose only action is to tell an operator to
// look.
func ScanKeys(ctx context.Context, client *redisutil.Client, pattern string) ([]string, error) {
	var (
		keys   []string
		cursor uint64
	)
	for {
		batch, next, err := client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if next == 0 {
			return keys, nil
		}
		cursor = next
	}
}
