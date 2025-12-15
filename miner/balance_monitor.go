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
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// BalanceMonitorConfig contains configuration for balance monitoring.
type BalanceMonitorConfig struct {
	// CheckInterval is how often to check balances and stakes.
	CheckInterval time.Duration

	// BalanceThresholdUpokt is the minimum balance in uPOKT before triggering warnings.
	// If 0, balance warnings are disabled.
	BalanceThresholdUpokt int64

	// StakeWarningProofThreshold is the number of missed proofs remaining before triggering a warning.
	// Default: 1000 (warn when less than 1000 missed proofs away from auto-unstake)
	StakeWarningProofThreshold int64

	// StakeCriticalProofThreshold is the number of missed proofs remaining before triggering a critical alert.
	// Default: 100 (critical when less than 100 missed proofs away from auto-unstake)
	StakeCriticalProofThreshold int64
}

// BalanceMonitor monitors supplier account balances and stake health.
// This is a leader-only operation to avoid duplicate alerts.
type BalanceMonitor struct {
	logger              logging.Logger
	config              BalanceMonitorConfig
	bankClient          client.BankQueryClient
	supplierClient      client.SupplierQueryClient
	supplierParamsCache interface {
		GetSupplierParams(ctx context.Context) (*suppliertypes.Params, error)
	}
	proofParamsCache interface {
		Get(ctx context.Context, force ...bool) (*prooftypes.Params, error)
	}
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
	supplierParamsCache interface {
		GetSupplierParams(ctx context.Context) (*suppliertypes.Params, error)
	},
	proofParamsCache interface {
		Get(ctx context.Context, force ...bool) (*prooftypes.Params, error)
	},
	supplierManager *SupplierManager,
	globalLeader *leader.GlobalLeaderElector,
) *BalanceMonitor {
	return &BalanceMonitor{
		logger:              logging.ForComponent(logger, logging.ComponentBalanceMonitor),
		config:              config,
		bankClient:          bankClient,
		supplierClient:      supplierClient,
		supplierParamsCache: supplierParamsCache,
		proofParamsCache:    proofParamsCache,
		supplierManager:     supplierManager,
		globalLeader:        globalLeader,
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
		Int64("stake_warning_proof_threshold", m.config.StakeWarningProofThreshold).
		Int64("stake_critical_proof_threshold", m.config.StakeCriticalProofThreshold).
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
	// Query supplier stake FIRST to determine if we should monitor balance
	// Pre-loaded keys (not yet staked) should not trigger balance alerts
	supplier, err := m.supplierClient.GetSupplier(ctx, supplierAddr)
	isStaked := false
	if err != nil {
		// Check if it's a NotFound error (supplier not staked)
		if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
			m.logger.Debug().
				Str("supplier", supplierAddr).
				Msg("supplier not staked (skipping balance/stake monitoring)")
			// Skip balance alerts for unstaked suppliers (pre-loaded keys)
			return
		} else {
			// Other errors (network, timeout, etc.) - still try to check balance
			m.logger.Warn().
				Err(err).
				Str("supplier", supplierAddr).
				Msg("failed to query supplier stake, will still check balance")
			supplierMonitorErrors.WithLabelValues(supplierAddr, "stake_query").Inc()
		}
	} else {
		isStaked = true
	}

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

	// Only check balance threshold if supplier is actually staked
	// Pre-loaded keys should not trigger balance alerts
	if isStaked && m.config.BalanceThresholdUpokt > 0 && balanceUpokt < m.config.BalanceThresholdUpokt {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Int64("balance_upokt", balanceUpokt).
			Int64("threshold_upokt", m.config.BalanceThresholdUpokt).
			Msg("CRITICAL: Supplier balance below threshold")
		supplierBalanceCriticalAlerts.WithLabelValues(supplierAddr).Inc()
		supplierBalanceHealthStatus.WithLabelValues(supplierAddr).Set(0) // 0 = critical
	} else if isStaked && m.config.BalanceThresholdUpokt > 0 && balanceUpokt < m.config.BalanceThresholdUpokt*2 {
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

	// Process stake information if supplier is staked
	if !isStaked {
		return
	}

	// Extract stake in uPOKT
	stakeUpokt := supplier.GetStake().Amount.Int64()
	supplierStakeUpokt.WithLabelValues(supplierAddr).Set(float64(stakeUpokt))

	// Query supplier module params from cache to get actual minimum stake requirement
	// This avoids 1000+ chain queries per monitoring interval
	params, err := m.supplierParamsCache.GetSupplierParams(ctx)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("supplier", supplierAddr).
			Msg("failed to get cached supplier params, skipping stake health check")
		supplierMonitorErrors.WithLabelValues(supplierAddr, "params_cache").Inc()
		return
	}

	// Get actual minimum stake from protocol params (not hardcoded!)
	minStakeUpokt := params.MinStake.Amount.Int64()

	// Get proof missing penalty from proof params cache
	proofParams, err := m.proofParamsCache.Get(ctx)
	if err != nil {
		m.logger.Warn().
			Err(err).
			Str("supplier", supplierAddr).
			Msg("failed to get cached proof params, skipping stake health check")
		supplierMonitorErrors.WithLabelValues(supplierAddr, "proof_params_cache").Inc()
		return
	}

	proofMissingPenalty := proofParams.GetProofMissingPenalty()
	if proofMissingPenalty == nil || proofMissingPenalty.Amount.Int64() == 0 {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Msg("proof missing penalty is zero, skipping stake health check")
		return
	}

	penaltyUpokt := proofMissingPenalty.Amount.Int64()

	// Calculate missed proofs remaining before auto-unstake
	// Formula: missedProofsRemaining = (stake - minStake) / penaltyPerProof
	bufferUpokt := stakeUpokt - minStakeUpokt
	missedProofsRemaining := bufferUpokt / penaltyUpokt

	// Calculate stake health ratio for metrics
	stakeHealthRatio := float64(stakeUpokt) / float64(minStakeUpokt)
	supplierStakeHealthRatio.WithLabelValues(supplierAddr).Set(stakeHealthRatio)

	// Check stake health based on missed proofs remaining
	if missedProofsRemaining < m.config.StakeCriticalProofThreshold {
		m.logger.Error().
			Str("supplier", supplierAddr).
			Int64("stake_upokt", stakeUpokt).
			Int64("min_stake_upokt", minStakeUpokt).
			Int64("buffer_upokt", bufferUpokt).
			Int64("penalty_per_proof_upokt", penaltyUpokt).
			Int64("missed_proofs_remaining", missedProofsRemaining).
			Int64("critical_threshold", m.config.StakeCriticalProofThreshold).
			Float64("stake_health_ratio", stakeHealthRatio).
			Msg("CRITICAL: Supplier stake critically low - very close to auto-unstake!")
		supplierStakeCriticalAlerts.WithLabelValues(supplierAddr).Inc()
	} else if missedProofsRemaining < m.config.StakeWarningProofThreshold {
		m.logger.Warn().
			Str("supplier", supplierAddr).
			Int64("stake_upokt", stakeUpokt).
			Int64("min_stake_upokt", minStakeUpokt).
			Int64("buffer_upokt", bufferUpokt).
			Int64("penalty_per_proof_upokt", penaltyUpokt).
			Int64("missed_proofs_remaining", missedProofsRemaining).
			Int64("warning_threshold", m.config.StakeWarningProofThreshold).
			Float64("stake_health_ratio", stakeHealthRatio).
			Msg("WARNING: Supplier stake below healthy buffer threshold")
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
