package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/alitto/pond/v2"
	abcitypes "github.com/cometbft/cometbft/abci/types"
	"github.com/cometbft/cometbft/rpc/client/http"

	haclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SettlementMonitor watches for EventClaimSettled events from the blockchain
// and emits metrics for on-chain claim settlement results.
//
// This provides visibility into:
// - Proven vs Invalid vs Expired claims
// - Actual uPOKT earned from settlements
// - Settlement latency (blocks between session end and settlement)
type SettlementMonitor struct {
	logger          logging.Logger
	blockSubscriber *haclient.BlockSubscriber
	rpcClient       *http.HTTP
	suppliers       map[string]bool // Set of supplier addresses to monitor
	sessionStore    SessionStore    // For updating settlement metadata (optional)
	mu              sync.RWMutex
	workerPool      pond.Pool // Subpool for processing block_results (1GB+ in mainnet)
	ctx             context.Context
	cancel          context.CancelFunc
}

// NewSettlementMonitor creates a new settlement monitor.
// The masterPool parameter is required for creating settlement processing subpool.
// The sessionStore parameter is optional - if nil, settlement metadata won't be updated.
func NewSettlementMonitor(
	logger logging.Logger,
	blockSubscriber *haclient.BlockSubscriber,
	rpcClient *http.HTTP,
	suppliers []string,
	masterPool pond.Pool,
	sessionStore SessionStore,
) *SettlementMonitor {
	// Build supplier set for fast lookup
	supplierSet := make(map[string]bool, len(suppliers))
	for _, supplier := range suppliers {
		supplierSet[supplier] = true
	}

	// Create settlement processing subpool from master pool
	// Subpool size: 2 workers (settlement events are rare, 1 per session)
	// Using a subpool ensures settlement processing doesn't lock claim/proof submission
	settlementSubpool := masterPool.NewSubpool(2)

	return &SettlementMonitor{
		logger:          logging.ForComponent(logger, "settlement_monitor"),
		blockSubscriber: blockSubscriber,
		rpcClient:       rpcClient,
		suppliers:       supplierSet,
		sessionStore:    sessionStore,
		workerPool:      settlementSubpool,
	}
}

// Start begins monitoring for settlement events.
func (sm *SettlementMonitor) Start(ctx context.Context) error {
	sm.ctx, sm.cancel = context.WithCancel(ctx)

	// Subscribe to block events
	blockEvents := sm.blockSubscriber.Subscribe(sm.ctx, 100)

	// Submit main event loop to pond worker subpool (no uncontrolled goroutines)
	sm.workerPool.Submit(func() {
		sm.logger.Info().Msg("settlement monitor started")

		for {
			select {
			case <-sm.ctx.Done():
				sm.logger.Info().Msg("settlement monitor stopped")
				return

			case block, ok := <-blockEvents:
				if !ok {
					sm.logger.Warn().Msg("block events channel closed")
					return
				}

				// Submit block processing to worker subpool to avoid blocking on large block_results (1GB+ in mainnet)
				blockHeight := block.Height()
				sm.workerPool.Submit(func() {
					// Query height-1 to avoid race condition with ABCI indexing
					// Block events from WebSocket arrive real-time, but block_results
					// are indexed asynchronously by the ABCI module.
					// Querying the previous block ensures data is indexed.
					if blockHeight > 1 {
						sm.processBlock(blockHeight - 1)
					}
				})
			}
		}
	})

	return nil
}

// Close stops the settlement monitor.
func (sm *SettlementMonitor) Close() {
	if sm.cancel != nil {
		sm.cancel()
	}

	// Stop settlement subpool gracefully (drains queued tasks including event loop and block_results queries)
	if sm.workerPool != nil {
		sm.workerPool.StopAndWait()
	}

	sm.logger.Info().Msg("settlement monitor closed")
}

// AddSupplier adds a supplier address to monitor.
func (sm *SettlementMonitor) AddSupplier(supplier string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.suppliers[supplier] = true
}

// RemoveSupplier removes a supplier address from monitoring.
func (sm *SettlementMonitor) RemoveSupplier(supplier string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.suppliers, supplier)
}

// processBlock queries block_results for the given height and processes settlement events.
// This function implements infinite retry with exponential backoff to ensure settlement
// data is never lost. Only stops on context cancellation (shutdown).
func (sm *SettlementMonitor) processBlock(height int64) {
	const (
		initialRetryDelay = 2 * time.Second
		maxRetryDelay     = 30 * time.Second
		backoffMultiplier = 1.5
	)

	retryDelay := initialRetryDelay
	attempt := 0

	for {
		// Check for shutdown
		select {
		case <-sm.ctx.Done():
			sm.logger.Warn().
				Int64("height", height).
				Int("attempt", attempt).
				Msg("shutting down, abandoning block_results query")
			return
		default:
		}

		attempt++

		// Query block_results for this height
		result, err := sm.rpcClient.BlockResults(sm.ctx, &height)
		if err != nil {
			sm.logger.Warn().
				Err(err).
				Int64("height", height).
				Int("attempt", attempt).
				Dur("retry_in", retryDelay).
				Msg("failed to query block_results, will retry")

			// Record metric for retry
			RecordBlockResultsRetry(height, attempt)

			// Wait before retry with exponential backoff
			select {
			case <-sm.ctx.Done():
				sm.logger.Warn().
					Int64("height", height).
					Msg("shutting down during retry backoff")
				return
			case <-time.After(retryDelay):
				// Increase retry delay with exponential backoff
				retryDelay = time.Duration(float64(retryDelay) * backoffMultiplier)
				if retryDelay > maxRetryDelay {
					retryDelay = maxRetryDelay
				}
				continue
			}
		}

		// Success! Process all settlement events in this block
		sm.logger.Debug().
			Int64("height", height).
			Int("attempt", attempt).
			Int("events", len(result.FinalizeBlockEvents)).
			Msg("successfully queried block_results")

		for _, event := range result.FinalizeBlockEvents {
			switch event.Type {
			case "pocket.tokenomics.EventClaimSettled":
				sm.handleClaimSettled(event.Attributes, height)
			case "pocket.tokenomics.EventClaimExpired":
				sm.handleClaimExpired(event.Attributes, height)
			case "pocket.tokenomics.EventSupplierSlashed":
				sm.handleSupplierSlashed(event.Attributes, height)
			case "pocket.tokenomics.EventClaimDiscarded":
				sm.handleClaimDiscarded(event.Attributes, height)
			}
		}

		return
	}
}

// handleClaimSettled processes a single EventClaimSettled event.
func (sm *SettlementMonitor) handleClaimSettled(attributes []abcitypes.EventAttribute, settlementHeight int64) {
	// Extract attributes
	attrs := make(map[string]string)
	for _, attr := range attributes {
		attrs[attr.Key] = attr.Value
	}

	// Extract supplier address
	supplier := extractStringValue(attrs["supplier_operator_address"])

	// Check if this is one of our suppliers
	sm.mu.RLock()
	isOurs := sm.suppliers[supplier]
	sm.mu.RUnlock()

	if !isOurs {
		return // Not one of our suppliers, skip
	}

	// Extract other attributes
	serviceID := extractStringValue(attrs["service_id"])
	statusInt := extractIntValue(attrs["claim_proof_status_int"])
	numRelays := extractIntValue(attrs["num_relays"])
	computeUnits := extractIntValue(attrs["num_claimed_compute_units"])
	sessionEndHeight := extractIntValue(attrs["session_end_block_height"])

	// Map status int to status string
	// 0 = CLAIMED, 1 = PROVEN, 2 = (not used), 3 = EXPIRED
	// ClaimProofStage: CLAIMED=0, PROVEN=1, SETTLED=2, EXPIRED=3
	var status string
	switch statusInt {
	case 1:
		status = "proven"
	case 2:
		status = "invalid" // Not used in practice but handle it
	case 3:
		status = "expired"
	default:
		status = "unknown"
	}

	// Extract uPOKT earned (from reward_distribution JSON)
	upoktEarned := int64(0)
	if rewardDistJSON := attrs["reward_distribution"]; rewardDistJSON != "" {
		upoktEarned = sm.extractSupplierReward(rewardDistJSON, supplier)
	}

	sm.logger.Info().
		Str("supplier", supplier).
		Str("service_id", serviceID).
		Str("status", status).
		Int64("num_relays", numRelays).
		Int64("compute_units", computeUnits).
		Int64("upokt_earned", upoktEarned).
		Int64("session_end_height", sessionEndHeight).
		Int64("settlement_height", settlementHeight).
		Msg("claim settled on-chain")

	// Record metrics
	RecordClaimSettled(supplier, serviceID, status, numRelays, computeUnits, upoktEarned, sessionEndHeight, settlementHeight)

	// OPTIONAL: Update settlement metadata in session snapshot (if session still exists in Redis)
	if sm.sessionStore != nil {
		// Try to extract session_id from attributes (may not be present)
		sessionID := extractStringValue(attrs["session_id"])
		if sessionID != "" {
			outcome := fmt.Sprintf("settled_%s", status) // "settled_proven", "settled_invalid", "settled_expired"
			if err := sm.sessionStore.UpdateSettlementMetadata(sm.ctx, sessionID, outcome, settlementHeight); err != nil {
				// Non-critical: session may already be cleaned up (expected for old sessions)
				sm.logger.Debug().
					Err(err).
					Str("session_id", sessionID).
					Msg("failed to update settlement metadata (session may be cleaned up)")
			}
		}
	}
}

// extractSupplierReward parses the reward_distribution JSON and extracts the supplier's reward amount.
// reward_distribution format: {"address1":"10upokt","address2":"20upokt"}
func (sm *SettlementMonitor) extractSupplierReward(rewardDistJSON, supplier string) int64 {
	// Parse JSON
	var distribution map[string]string
	if err := json.Unmarshal([]byte(rewardDistJSON), &distribution); err != nil {
		sm.logger.Warn().
			Err(err).
			Str("reward_distribution", rewardDistJSON).
			Msg("failed to parse reward distribution JSON")
		return 0
	}

	// Find supplier's reward
	reward, ok := distribution[supplier]
	if !ok {
		return 0
	}

	// Parse "70upokt" -> 70
	return parseUpoktAmount(reward)
}

// Helper functions to extract values from event attributes

func extractStringValue(quoted string) string {
	// Remove quotes: "value" -> value
	if len(quoted) >= 2 && quoted[0] == '"' && quoted[len(quoted)-1] == '"' {
		return quoted[1 : len(quoted)-1]
	}
	return quoted
}

func extractIntValue(str string) int64 {
	// Remove quotes if present
	str = extractStringValue(str)

	val, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return 0
	}
	return val
}

func parseUpoktAmount(amount string) int64 {
	// Parse "70upokt" -> 70
	if len(amount) == 0 {
		return 0
	}

	// Remove "upokt" suffix
	if len(amount) > 5 && amount[len(amount)-5:] == "upokt" {
		amount = amount[:len(amount)-5]
	}

	val, err := strconv.ParseInt(amount, 10, 64)
	if err != nil {
		return 0
	}
	return val
}

// handleClaimExpired processes a single EventClaimExpired event.
// Emitted when a claim expires without a proof (PROOF_MISSING or PROOF_INVALID).
func (sm *SettlementMonitor) handleClaimExpired(attributes []abcitypes.EventAttribute, settlementHeight int64) {
	// Extract attributes
	attrs := make(map[string]string)
	for _, attr := range attributes {
		attrs[attr.Key] = attr.Value
	}

	// Extract supplier address
	supplier := extractStringValue(attrs["supplier_operator_address"])

	// Check if this is one of our suppliers
	sm.mu.RLock()
	isOurs := sm.suppliers[supplier]
	sm.mu.RUnlock()

	if !isOurs {
		return // Not one of our suppliers, skip
	}

	// Extract other attributes
	serviceID := extractStringValue(attrs["service_id"])
	expirationReasonRaw := extractStringValue(attrs["expiration_reason"])
	numRelays := extractIntValue(attrs["num_relays"])
	computeUnits := extractIntValue(attrs["num_claimed_compute_units"])
	sessionEndHeight := extractIntValue(attrs["session_end_block_height"])
	claimedUpoktStr := extractStringValue(attrs["claimed_upokt"])

	// Parse claimed uPOKT amount (e.g., "70upokt" -> 70)
	claimedUpokt := parseUpoktAmount(claimedUpoktStr)

	// Map expiration reason string from blockchain to lowercase metric label
	// Blockchain sends: "PROOF_MISSING", "PROOF_INVALID", etc.
	var reason string
	switch expirationReasonRaw {
	case "PROOF_MISSING":
		reason = "proof_missing"
	case "PROOF_INVALID":
		reason = "proof_invalid"
	default:
		reason = "unspecified"
	}

	sm.logger.Warn().
		Str("supplier", supplier).
		Str("service_id", serviceID).
		Str("expiration_reason", reason).
		Int64("num_relays", numRelays).
		Int64("compute_units", computeUnits).
		Int64("claimed_upokt", claimedUpokt).
		Int64("session_end_height", sessionEndHeight).
		Int64("settlement_height", settlementHeight).
		Msg("claim expired on-chain")

	// Record metrics
	RecordClaimExpired(supplier, serviceID, reason, numRelays, computeUnits, claimedUpokt, sessionEndHeight, settlementHeight)
}

// handleSupplierSlashed processes a single EventSupplierSlashed event.
// Emitted when a supplier is slashed for missing proof submission.
func (sm *SettlementMonitor) handleSupplierSlashed(attributes []abcitypes.EventAttribute, settlementHeight int64) {
	// Extract attributes
	attrs := make(map[string]string)
	for _, attr := range attributes {
		attrs[attr.Key] = attr.Value
	}

	// Extract supplier address
	supplier := extractStringValue(attrs["supplier_operator_address"])

	// Check if this is one of our suppliers
	sm.mu.RLock()
	isOurs := sm.suppliers[supplier]
	sm.mu.RUnlock()

	if !isOurs {
		return // Not one of our suppliers, skip
	}

	// Extract other attributes
	serviceID := extractStringValue(attrs["service_id"])
	penalty := extractStringValue(attrs["proof_missing_penalty"])
	sessionEndHeight := extractIntValue(attrs["session_end_block_height"])

	sm.logger.Error().
		Str("supplier", supplier).
		Str("service_id", serviceID).
		Str("penalty", penalty).
		Int64("session_end_height", sessionEndHeight).
		Int64("settlement_height", settlementHeight).
		Msg("supplier slashed on-chain")

	// Record metrics
	RecordSupplierSlashed(supplier, serviceID, penalty, sessionEndHeight, settlementHeight)
}

// handleClaimDiscarded processes a single EventClaimDiscarded event.
// Emitted when a claim is discarded due to unexpected errors to prevent chain halts.
func (sm *SettlementMonitor) handleClaimDiscarded(attributes []abcitypes.EventAttribute, settlementHeight int64) {
	// Extract attributes
	attrs := make(map[string]string)
	for _, attr := range attributes {
		attrs[attr.Key] = attr.Value
	}

	// Extract supplier address
	supplier := extractStringValue(attrs["supplier_operator_address"])

	// Check if this is one of our suppliers
	sm.mu.RLock()
	isOurs := sm.suppliers[supplier]
	sm.mu.RUnlock()

	if !isOurs {
		return // Not one of our suppliers, skip
	}

	// Extract other attributes
	serviceID := extractStringValue(attrs["service_id"])
	errorMsg := extractStringValue(attrs["error"])
	sessionEndHeight := extractIntValue(attrs["session_end_block_height"])

	sm.logger.Error().
		Str("supplier", supplier).
		Str("service_id", serviceID).
		Str("error", errorMsg).
		Int64("session_end_height", sessionEndHeight).
		Int64("settlement_height", settlementHeight).
		Msg("claim discarded on-chain")

	// Record metrics
	RecordClaimDiscarded(supplier, serviceID, errorMsg, sessionEndHeight, settlementHeight)
}
