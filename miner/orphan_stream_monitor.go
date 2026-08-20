package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// defaultOrphanStreamCheckInterval paces a sweep that answers a slow question.
//
// A stream becomes orphaned when a supplier is decommissioned for good, which
// happens on the timescale of operator decisions, not of blocks. The sweep is a
// SCAN over every relay stream plus one over the supplier cache, so it is not
// free; running it often would cost real Redis work to re-learn something that
// almost never changes.
const defaultOrphanStreamCheckInterval = time.Hour

// OrphanStreamMonitor reports relay streams whose supplier this deployment no
// longer has any record of.
//
// It exists because relay streams no longer expire. A clock was the wrong
// instrument for ending a supplier's lane -- it fired mid-session and took
// un-consumed relays and the pending entries list with it -- so the lane now
// lives as long as the supplier does. The cost of that trade is that a supplier
// decommissioned for good leaves its lane behind, and this makes those lanes
// visible instead of leaving them to be discovered by someone reading key
// counts.
//
// It only ever COUNTS. Deleting a stream key deletes its consumer group and that
// group's pending entries with it, and the consumer recreates the group empty on
// its next connect, so an automatic delete would destroy unpaid relays with no
// trace. Cleanup is an operator decision, taken with
// `pocket-relay-miner redis streams --orphaned`.
type OrphanStreamMonitor struct {
	logger        logging.Logger
	redisClient   *redisutil.Client
	globalLeader  *leader.GlobalLeaderElector
	checkInterval time.Duration

	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.Mutex
	closed   bool
}

// NewOrphanStreamMonitor creates the monitor. A non-positive interval falls back
// to defaultOrphanStreamCheckInterval.
func NewOrphanStreamMonitor(
	logger logging.Logger,
	redisClient *redisutil.Client,
	globalLeader *leader.GlobalLeaderElector,
	checkInterval time.Duration,
) *OrphanStreamMonitor {
	if checkInterval <= 0 {
		checkInterval = defaultOrphanStreamCheckInterval
	}
	return &OrphanStreamMonitor{
		logger:        logging.ForComponent(logger, "orphan_stream_monitor"),
		redisClient:   redisClient,
		globalLeader:  globalLeader,
		checkInterval: checkInterval,
	}
}

// Start begins the sweep loop.
func (m *OrphanStreamMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("orphan stream monitor is closed")
	}
	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	m.wg.Add(1)
	// Wrapped rather than a bare `go`: a panic in a long-lived goroutine would
	// otherwise take the whole miner down, and internal/conventions freezes the
	// bare ones that predate that rule so no new ones appear.
	go logging.RecoverGoRoutine(m.logger, "orphan_stream_monitor", m.worker)(m.ctx)
	return nil
}

func (m *OrphanStreamMonitor) worker(ctx context.Context) {
	defer m.wg.Done()

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	// No sweep on the first tick's worth of startup: a replica that has just
	// become leader has not necessarily finished registering its own suppliers,
	// and counting then would report every one of them as an orphan.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweep(ctx)
		}
	}
}

// sweep counts orphaned streams and publishes the count.
//
// Leader-gated so the gauge has ONE writer. Every replica sees the same Redis,
// so letting them all report would multiply the scan cost by the replica count
// to learn the same number.
func (m *OrphanStreamMonitor) sweep(ctx context.Context) {
	if !m.globalLeader.IsLeader() {
		return
	}

	orphans, err := OrphanStreamAddresses(ctx, m.redisClient, func(ctx context.Context, pattern string) ([]string, error) {
		return ScanKeys(ctx, m.redisClient, pattern)
	})
	if err != nil {
		if ctx.Err() == nil {
			m.logger.Warn().Err(err).Msg("failed to sweep for orphaned relay streams")
		}
		return
	}

	RecordOrphanedStreams(len(orphans))

	if len(orphans) == 0 {
		return
	}

	// Info, not Warn: an orphaned stream is bookkeeping, not lost relays, and
	// this fires once per sweep rather than once per stream. The addresses are
	// logged because the next question an operator asks is "which ones", and the
	// gauge cannot answer it -- an address is far too high-cardinality to be a
	// Prometheus label.
	m.logger.Info().
		Int("orphaned_streams", len(orphans)).
		Strs("suppliers", orphans).
		Msg("relay streams exist for suppliers this deployment no longer knows; " +
			"inspect with `redis streams --orphaned`")
}

// Close stops the monitor. It is idempotent.
func (m *OrphanStreamMonitor) Close() error {
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
	return nil
}
