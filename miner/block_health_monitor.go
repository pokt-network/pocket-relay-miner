package miner

import (
	"context"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

// BlockHealthMonitor monitors block time and alerts when blocks are slow.
// Only runs on the global leader to avoid duplicate alerts.
type BlockHealthMonitor struct {
	logger            logging.Logger
	blockClient       client.BlockClient
	globalLeader      *leader.GlobalLeaderElector
	blockTimeSeconds  int64
	slownessThreshold float64

	// State tracking
	lastBlockTime        time.Time
	consecutiveSlowCount int
	inSlowMode           bool

	// Lifecycle
	mu       sync.Mutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// BlockHealthMonitorConfig contains configuration for block health monitoring.
type BlockHealthMonitorConfig struct {
	// BlockTimeSeconds is the expected block time.
	BlockTimeSeconds int64

	// SlownessThreshold is the multiplier for determining slow blocks.
	// Default: 1.5 (50% slower than expected)
	SlownessThreshold float64
}

// NewBlockHealthMonitor creates a new block health monitor.
func NewBlockHealthMonitor(
	logger logging.Logger,
	blockClient client.BlockClient,
	globalLeader *leader.GlobalLeaderElector,
	config BlockHealthMonitorConfig,
) *BlockHealthMonitor {
	if config.SlownessThreshold == 0 {
		config.SlownessThreshold = 1.5
	}

	return &BlockHealthMonitor{
		logger:            logging.ForComponent(logger, logging.ComponentBlockHealth),
		blockClient:       blockClient,
		globalLeader:      globalLeader,
		blockTimeSeconds:  config.BlockTimeSeconds,
		slownessThreshold: config.SlownessThreshold,
	}
}

// Start begins monitoring block times.
func (m *BlockHealthMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}

	ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Set configured block time metric
	configuredBlockTimeSeconds.Set(float64(m.blockTimeSeconds))

	m.wg.Add(1)
	go m.monitorLoop(ctx)

	m.logger.Info().
		Int64("block_time_seconds", m.blockTimeSeconds).
		Float64("slowness_threshold", m.slownessThreshold).
		Msg("block health monitor started (leader-only)")

	return nil
}

// monitorLoop is the main monitoring loop.
func (m *BlockHealthMonitor) monitorLoop(ctx context.Context) {
	defer m.wg.Done()

	// Try to use the new Subscribe() method for independent channel
	// Falls back to BlockEvents() for backward compatibility
	var blockEventsCh <-chan client.Block

	if subscriber, ok := m.blockClient.(interface {
		Subscribe(ctx context.Context, bufferSize int) <-chan client.Block
	}); ok {
		blockEventsCh = subscriber.Subscribe(ctx, 50) // 50-block buffer for health monitoring
		m.logger.Info().Msg("using Subscribe() for block events (fan-out mode)")
	} else if legacySubscriber, ok := m.blockClient.(interface {
		BlockEvents() <-chan client.Block
	}); ok {
		blockEventsCh = legacySubscriber.BlockEvents()
		m.logger.Info().Msg("using BlockEvents() for block events (legacy mode)")
	} else {
		m.logger.Error().Msg("block client does not support Subscribe() or BlockEvents() - block health monitoring disabled")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return

		case block, ok := <-blockEventsCh:
			if !ok {
				// Channel closed
				return
			}

			// Only monitor if we're the leader
			if !m.globalLeader.IsLeader() {
				continue
			}

			m.checkBlockTiming(block)
		}
	}
}

// checkBlockTiming checks if the block time is within expected bounds.
func (m *BlockHealthMonitor) checkBlockTiming(block client.Block) {
	now := time.Now()

	// Skip first block (no previous time to compare)
	if m.lastBlockTime.IsZero() {
		m.lastBlockTime = now
		return
	}

	// Calculate actual time between blocks
	actualInterval := now.Sub(m.lastBlockTime)
	m.lastBlockTime = now

	// Update metrics
	currentBlockIntervalSeconds.Set(actualInterval.Seconds())

	// Calculate expected time
	expectedInterval := time.Duration(m.blockTimeSeconds) * time.Second
	slowThreshold := time.Duration(float64(expectedInterval) * m.slownessThreshold)

	// Check if block is slow
	if actualInterval > slowThreshold {
		// Slow block detected
		m.consecutiveSlowCount++
		fullnodeSlowBlocksTotal.Inc()
		fullnodeSlowBlocksConsecutive.Set(float64(m.consecutiveSlowCount))

		// Log WARNING after 3 consecutive slow blocks
		if m.consecutiveSlowCount >= 3 && !m.inSlowMode {
			m.inSlowMode = true
			m.logger.Warn().
				Int64("height", block.Height()).
				Dur("actual_interval", actualInterval).
				Dur("expected_interval", expectedInterval).
				Dur("threshold", slowThreshold).
				Int("consecutive_slow_blocks", m.consecutiveSlowCount).
				Msg("FULLNODE SLOWNESS DETECTED - blocks consistently slower than expected")
		} else if m.consecutiveSlowCount < 3 {
			// Log at debug level for isolated slow blocks
			m.logger.Debug().
				Int64("height", block.Height()).
				Dur("actual_interval", actualInterval).
				Dur("expected_interval", expectedInterval).
				Dur("threshold", slowThreshold).
				Msg("slow block detected")
		}
	} else {
		// Block is normal speed
		if m.inSlowMode {
			// Recovered from slow mode
			m.logger.Info().
				Int64("height", block.Height()).
				Dur("actual_interval", actualInterval).
				Dur("expected_interval", expectedInterval).
				Int("was_slow_for_blocks", m.consecutiveSlowCount).
				Msg("block times returned to normal")
			m.inSlowMode = false
		}

		// Reset consecutive counter
		m.consecutiveSlowCount = 0
		fullnodeSlowBlocksConsecutive.Set(0)
	}
}

// Close stops the block health monitor.
func (m *BlockHealthMonitor) Close() error {
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

	m.logger.Info().Msg("block health monitor stopped")
	return nil
}
