package leader

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	// redisHealthCheckInterval is how often we poll Redis INFO MEMORY.
	redisHealthCheckInterval = 30 * time.Second

	// redisMemoryWarningThreshold triggers a WARN log when usage exceeds 90%.
	redisMemoryWarningThreshold = 0.9

	// ComponentRedisHealth is the logging component name.
	ComponentRedisHealth = "redis_health_monitor"
)

// RedisHealthMonitor periodically polls Redis INFO MEMORY and exposes
// memory usage metrics. Runs on ALL replicas (not just leader) because
// any replica needs visibility into Redis memory state — especially
// standbys that cannot acquire leadership during OOM.
type RedisHealthMonitor struct {
	logger      logging.Logger
	redisClient *redisutil.Client

	// Lifecycle
	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewRedisHealthMonitor creates a new Redis health monitor.
func NewRedisHealthMonitor(
	logger logging.Logger,
	redisClient *redisutil.Client,
) *RedisHealthMonitor {
	return &RedisHealthMonitor{
		logger:      logging.ForComponent(logger, ComponentRedisHealth),
		redisClient: redisClient,
	}
}

// Start begins the periodic Redis memory health check loop.
func (m *RedisHealthMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}

	ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	m.wg.Add(1)
	go m.monitorLoop(ctx)

	m.logger.Info().
		Dur("interval", redisHealthCheckInterval).
		Float64("warning_threshold", redisMemoryWarningThreshold).
		Msg("Redis health monitor started")

	return nil
}

// monitorLoop polls Redis INFO MEMORY at regular intervals.
func (m *RedisHealthMonitor) monitorLoop(ctx context.Context) {
	defer m.wg.Done()

	// Check immediately on startup
	m.checkMemory(ctx)

	ticker := time.NewTicker(redisHealthCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkMemory(ctx)
		}
	}
}

// checkMemory runs INFO MEMORY and updates metrics.
func (m *RedisHealthMonitor) checkMemory(ctx context.Context) {
	infoResult, err := m.redisClient.Info(ctx, "memory").Result()
	if err != nil {
		m.logger.Warn().Err(err).Msg("failed to query Redis INFO MEMORY")
		return
	}

	usedMemory, maxMemory, err := parseMemoryInfo(infoResult)
	if err != nil {
		m.logger.Warn().Err(err).Msg("failed to parse Redis INFO MEMORY")
		return
	}

	// Update metrics
	redisUsedMemoryBytes.Set(float64(usedMemory))
	redisMaxMemoryBytes.Set(float64(maxMemory))

	if maxMemory > 0 {
		ratio := float64(usedMemory) / float64(maxMemory)
		redisMemoryUsageRatio.Set(ratio)

		if ratio > redisMemoryWarningThreshold {
			m.logger.Warn().
				Int64("used_memory_bytes", usedMemory).
				Int64("max_memory_bytes", maxMemory).
				Float64("usage_ratio", ratio).
				Msg("REDIS MEMORY HIGH - approaching maxmemory limit, OOM errors likely")
		}
	} else {
		// maxmemory=0 means no limit
		redisMemoryUsageRatio.Set(-1)
	}
}

// parseMemoryInfo extracts used_memory and maxmemory from Redis INFO MEMORY output.
func parseMemoryInfo(info string) (usedMemory, maxMemory int64, err error) {
	for _, line := range strings.Split(info, "\r\n") {
		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		switch key {
		case "used_memory":
			usedMemory, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, 0, err
			}
		case "maxmemory":
			maxMemory, err = strconv.ParseInt(value, 10, 64)
			if err != nil {
				return 0, 0, err
			}
		}
	}

	return usedMemory, maxMemory, nil
}

// Close stops the Redis health monitor.
func (m *RedisHealthMonitor) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil
	}
	m.closed = true

	if m.cancelFn != nil {
		m.cancelFn()
	}

	m.wg.Wait()

	m.logger.Info().Msg("Redis health monitor stopped")
	return nil
}
