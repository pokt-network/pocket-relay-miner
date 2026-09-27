package miner

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// storeMemoryScanBudget bounds one measurement of what a full Redis holds.
const storeMemoryScanBudget = 30 * time.Second

// RecordStoreMemoryOnClose measures, each time health closes, how much of Redis
// is the relay streams, how much the SMSTs and how much everything else. The miner
// pauses reading while the store is closed, so a store full of stream backlog is
// only freed by what deletes trees -- a proof -- and this is what tells the two
// cases apart. SCAN and MEMORY USAGE are reads, which Redis serves when full.
func RecordStoreMemoryOnClose(ctx context.Context, logger logging.Logger, client *redistransport.Client, health *redistransport.StoreHealth) {
	health.OnChange(func(operable bool) {
		if operable {
			return
		}
		go logging.RecoverGoRoutine(logger, "store_memory_on_close", func(c context.Context) {
			measureCtx, cancel := context.WithTimeout(c, storeMemoryScanBudget)
			defer cancel()
			streams, smst, used, err := measureStoreMemory(measureCtx, client)
			if err != nil {
				logger.Warn().Err(err).Msg("could not measure what Redis holds at store close")
				return
			}
			other := int64(used) - int64(streams) - int64(smst)
			storeMemoryAtClose.WithLabelValues("stream").Set(float64(streams))
			storeMemoryAtClose.WithLabelValues("smst").Set(float64(smst))
			storeMemoryAtClose.WithLabelValues("other").Set(float64(max(other, 0)))
			logger.Warn().
				Uint64("stream_bytes", streams).
				Uint64("smst_bytes", smst).
				Int64("other_bytes", max(other, 0)).
				Uint64("used_memory", used).
				Msg("Redis memory at store close, by key family")
		})(ctx)
	})
}

// measureStoreMemory sums MEMORY USAGE of the relay streams and of every SMST
// key, and reads used_memory.
func measureStoreMemory(ctx context.Context, client *redistransport.Client) (streams, smst, used uint64, err error) {
	kb := client.KB()
	if streams, err = sumKeyMemory(ctx, client, kb.StreamPattern()); err != nil {
		return 0, 0, 0, err
	}
	if smst, err = sumKeyMemory(ctx, client, kb.SMSTAllPattern()); err != nil {
		return 0, 0, 0, err
	}
	info, err := client.Info(ctx, "memory").Result()
	if err != nil {
		return 0, 0, 0, err
	}
	for _, line := range strings.Split(info, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "used_memory:"); ok {
			used, _ = strconv.ParseUint(v, 10, 64) //nolint:errcheck // a malformed value reads as zero, and other is clamped at zero
		}
	}
	return streams, smst, used, nil
}

// sumKeyMemory SCANs pattern and adds MEMORY USAGE of every key, one pipeline per
// page.
func sumKeyMemory(ctx context.Context, client *redistransport.Client, pattern string) (uint64, error) {
	var total uint64
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			return total, err
		}
		if len(keys) > 0 {
			pipe := client.Pipeline()
			cmds := make([]*redis.IntCmd, len(keys))
			for i, key := range keys {
				cmds[i] = pipe.MemoryUsage(ctx, key, 0)
			}
			if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
				return total, err
			}
			for _, cmd := range cmds {
				if n, err := cmd.Result(); err == nil && n > 0 {
					total += uint64(n)
				}
			}
		}
		cursor = next
		if cursor == 0 {
			return total, nil
		}
	}
}
