package miner

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/poktroll/pkg/client"
)

// BalanceMonitorConfig contains configuration for balance monitoring.
type BalanceMonitorConfig struct {
	// CheckInterval is how often to check balances and stakes.
	CheckInterval time.Duration

	// BalanceThresholdUpokt is the minimum balance in uPOKT before triggering warnings.
	// If 0, balance warnings are disabled.
	BalanceThresholdUpokt int64

	// StakeWarningRatio is the ratio of current stake to minimum required stake.
	// If current stake is below (min stake * ratio), a warning is triggered.
	// Default: 1.2 (20% buffer above minimum)
	StakeWarningRatio float64
}

// BalanceMonitor monitors supplier account balances and stake health.
// This is a leader-only operation to avoid duplicate alerts.
type BalanceMonitor struct {
	logger          logging.Logger
	config          BalanceMonitorConfig
	bankClient      client.BankQueryClient
	supplierClient  client.SupplierQueryClient
	supplierManager *SupplierManager
	globalLeader    *leader.GlobalLeaderElector

	// Lifecycle
	ctx      context.Context
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
	mu       sync.RWMutex
	closed   bool
}

// NewBalanceMonitor creates a new balance monitor.
func NewBalanceMonitor(
	logger logging.Logger,
	config BalanceMonitorConfig,
	bankClient client.BankQueryClient,
	supplierClient client.SupplierQueryClient,
	supplierManager *SupplierManager,
	globalLeader *leader.GlobalLeaderElector,
) *BalanceMonitor {
	return &BalanceMonitor{
		logger:          logging.ForComponent(logger, logging.ComponentBalanceMonitor),
		config:          config,
		bankClient:      bankClient,
		supplierClient:  supplierClient,
		supplierManager: supplierManager,
		globalLeader:    globalLeader,
	}
}

// Start begins the balance monitoring background process.
func (m *BalanceMonitor) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("balance monitor is closed")
	}

	m.ctx, m.cancelFn = context.WithCancel(ctx)
	m.mu.Unlock()

	// Start background monitoring worker
	m.wg.Add(1)
	go m.monitorWorker(m.ctx)

	m.logger.Info().
		Dur("check_interval", m.config.CheckInterval).
		Int64("balance_threshold_upokt", m.config.BalanceThresholdUpokt).
		Float64("stake_warning_ratio", m.config.StakeWarningRatio).
		Msg("balance monitor started")

	return nil
}

// monitorWorker runs periodic balance and stake checks.
func (m *BalanceMonitor) monitorWorker(ctx context.Context) {
	defer m.wg.Done()

	// Do initial check after a short delay to allow suppliers to initialize
	time.Sleep(30 * time.Second)
	m.checkAllSuppliers(ctx)

	ticker := time.NewTicker(m.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAllSuppliers(ctx)
		}
	}
}

// checkAllSuppliers checks balance and stake for all suppliers.
func (m *BalanceMonitor) checkAllSuppliers(ctx context.Context) {
	// Only run on leader to avoid duplicate alerts
	if !m.globalLeader.IsLeader() {
		m.logger.Debug().Msg("skipping balance check (not leader)")
		return
	}

	// Get all suppliers from manager
	suppliers := m.supplierManager.ListSuppliers()
	if len(suppliers) == 0 {
		m.logger.Debug().Msg("no suppliers to monitor")
		return
	}

	m.logger.Debug().
		Int("supplier_count", len(suppliers)).
		Msg("checking supplier balances and stakes")

	for _, supplierAddr := range suppliers {
		m.checkSupplier(ctx, supplierAddr)
	}
}

// checkSupplier checks balance and stake for a single supplier.
func (m *BalanceMonitor) checkSupplier(ctx context.Context, supplierAddr string) {
	// Query account balance using bank module
	balanceCoin, err := m.bankClient.GetBalance(ctx, supplierAddr)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("supplier", supplierAddr).
			Msg("failed to query account balance")
		supplierMonitorErrors.WithLabelValues(supplierAddr, "balance_query").Inc()
		return
	}

	// Extract balance in uPOKT (smallest denomination)
	balanceUpokt := int64(0)
	if balanceCoin != nil && balanceCoin.Denom == "upokt" {
		balanceUpokt = balanceCoin.Amount.Int64()
	}

	// Update balance metric
	supplierBalanceUpokt.WithLabelValues(supplierAddr).Set(float64(balanceUpokt))

	// Check balance threshold
	if m.config.BalanceThresholdUpokt > 0 && balanceUpokt < m.config.BalanceThresholdUpokt {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Int64("balance_upokt", balanceUpokt).
			Int64("threshold_upokt", m.config.BalanceThresholdUpokt).
			Msg("CRITICAL: Supplier balance below threshold")
		supplierBalanceCriticalAlerts.WithLabelValues(supplierAddr).Inc()
		supplierBalanceHealthStatus.WithLabelValues(supplierAddr).Set(0) // 0 = critical
	} else if m.config.BalanceThresholdUpokt > 0 && balanceUpokt < m.config.BalanceThresholdUpokt*2 {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Int64("balance_upokt", balanceUpokt).
			Int64("threshold_upokt", m.config.BalanceThresholdUpokt).
			Msg("WARNING: Supplier balance low (below 2x threshold)")
		supplierBalanceWarningAlerts.WithLabelValues(supplierAddr).Inc()
		supplierBalanceHealthStatus.WithLabelValues(supplierAddr).Set(1) // 1 = warning
	} else {
		supplierBalanceHealthStatus.WithLabelValues(supplierAddr).Set(2) // 2 = healthy
	}

	// Query supplier stake
	supplier, err := m.supplierClient.GetSupplier(ctx, supplierAddr)
	if err != nil {
		// Check if it's a NotFound error (supplier not staked)
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			m.logger.Debug().
				Str("supplier", supplierAddr).
				Msg("supplier not staked (cannot monitor stake)")
			supplierMonitorErrors.WithLabelValues(supplierAddr, "stake_query").Inc()
		} else {
			// Other errors (network, timeout, etc.)
			m.logger.Warn().
				Err(err).
				Str("supplier", supplierAddr).
				Msg("failed to query supplier stake")
			supplierMonitorErrors.WithLabelValues(supplierAddr, "stake_query").Inc()
		}
		return
	}

	// Extract stake in uPOKT
	stakeUpokt := supplier.GetStake().Amount.Int64()
	supplierStakeUpokt.WithLabelValues(supplierAddr).Set(float64(stakeUpokt))

	// Calculate minimum stake requirement
	// This should match the protocol's minimum stake for suppliers
	// For now, we use a simple heuristic based on service count
	// TODO: Query actual minimum stake from supplier module params when available
	numServices := len(supplier.GetServices())
	if numServices == 0 {
		numServices = 1 // Assume at least one service
	}

	// Estimate minimum stake (this is a rough heuristic - adjust based on protocol)
	// Typical minimum stake might be 15000 uPOKT per service
	// This should be confirmed with actual protocol parameters
	estimatedMinStake := int64(numServices) * 15000

	// Calculate warning threshold based on minimum stake
	// The warning ratio accounts for potential penalties that reduce stake
	requiredStake := estimatedMinStake
	warningThreshold := float64(requiredStake) * m.config.StakeWarningRatio

	stakeHealthRatio := float64(stakeUpokt) / float64(requiredStake)
	supplierStakeHealthRatio.WithLabelValues(supplierAddr).Set(stakeHealthRatio)

	// Check stake health
	if float64(stakeUpokt) < warningThreshold {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Int64("stake_upokt", stakeUpokt).
			Int64("required_stake_upokt", requiredStake).
			Float64("stake_health_ratio", stakeHealthRatio).
			Float64("warning_ratio", m.config.StakeWarningRatio).
			Msg("WARNING: Supplier stake close to auto-unstake threshold")
		supplierStakeWarningAlerts.WithLabelValues(supplierAddr).Inc()
	}

	m.logger.Debug().
		Str("supplier", supplierAddr).
		Int64("balance_upokt", balanceUpokt).
		Int64("stake_upokt", stakeUpokt).
		Float64("stake_health_ratio", stakeHealthRatio).
		Msg("supplier health check complete")
}

// Close gracefully shuts down the balance monitor.
func (m *BalanceMonitor) Close() error {
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

	m.logger.Info().Msg("balance monitor closed")
	return nil
}
