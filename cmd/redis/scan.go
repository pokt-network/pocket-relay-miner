package redis

import (
	"context"
	"fmt"
	"sync"

	goredis "github.com/redis/go-redis/v9"
)

// clusterAwareScanAllKeys enumerates every key matching pattern with
// non-blocking SCAN, working on both standalone/sentinel and Redis Cluster
// deployments.
//
// go-redis routes a plain SCAN on a ClusterClient to a single arbitrary
// node, so on cluster it silently misses every key hosted elsewhere — a
// cleanup or flush would report success while leaving most keys untouched.
// On cluster we therefore iterate every master via ForEachMaster.
func clusterAwareScanAllKeys(ctx context.Context, client *DebugRedisClient, pattern string) ([]string, error) {
	if cluster, ok := client.UniversalClient.(*goredis.ClusterClient); ok {
		var mu sync.Mutex
		var keys []string
		err := cluster.ForEachMaster(ctx, func(ctx context.Context, node *goredis.Client) error {
			nodeKeys, err := scanNode(ctx, node, pattern)
			if err != nil {
				return err
			}
			mu.Lock()
			keys = append(keys, nodeKeys...)
			mu.Unlock()
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("cluster-wide scan failed for pattern %q: %w", pattern, err)
		}
		return keys, nil
	}

	return scanNode(ctx, client, pattern)
}

// scanCmdable is the subset of the go-redis API scanNode needs, so it works
// for both a cluster node (*goredis.Client) and the wrapped client.
type scanCmdable interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *goredis.ScanCmd
}

func scanNode(ctx context.Context, c scanCmdable, pattern string) ([]string, error) {
	var cursor uint64
	var keys []string
	for {
		scanKeys, next, err := c.Scan(ctx, cursor, pattern, 1000).Result()
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

// pipelinedDelete deletes keys as single-key DELs inside pipelines of
// batchSize — one round trip per batch on standalone Redis and no CROSSSLOT
// errors on Redis Cluster (a single multi-key DEL spanning hash slots is
// rejected there). Returns the number of keys deleted; on error, reports how
// far it got.
func pipelinedDelete(ctx context.Context, client *DebugRedisClient, keys []string, batchSize int, progress func(deleted, total int)) (int, error) {
	if batchSize <= 0 {
		// Defensive: a non-positive batch size would never advance the loop.
		batchSize = 500
	}
	deleted := 0
	total := len(keys)
	for start := 0; start < total; start += batchSize {
		end := start + batchSize
		if end > total {
			end = total
		}
		batch := keys[start:end]
		if _, err := client.Pipelined(ctx, func(pipe goredis.Pipeliner) error {
			for _, k := range batch {
				pipe.Del(ctx, k)
			}
			return nil
		}); err != nil {
			return deleted, fmt.Errorf("batch delete failed at %d-%d: %w", start, end, err)
		}
		deleted += len(batch)
		if progress != nil {
			progress(deleted, total)
		}
	}
	return deleted, nil
}
