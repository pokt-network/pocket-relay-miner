package relayer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/poktroll/pkg/crypto"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// RelayProcessor handles the processing of relays including:
// - Deserializing relay requests
// - Building and signing relay responses
// - Calculating relay hashes
// - Checking mining difficulty
// - Publishing mined relays
type RelayProcessor interface {
	// ProcessRelay processes a served relay and returns a mined relay if applicable.
	// Returns nil if the relay doesn't meet mining difficulty.
	ProcessRelay(
		ctx context.Context,
		reqBody, respBody []byte,
		supplierAddr string,
		serviceID string,
		arrivalBlockHeight int64,
	) (*transport.MinedRelayMessage, error)

	// GetServiceDifficulty returns the mining difficulty for a service at a given session start height.
	GetServiceDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error)

	// SetDifficultyProvider sets the difficulty provider for mining checks.
	SetDifficultyProvider(provider DifficultyProvider)
}

// DifficultyProvider provides mining difficulty targets for services.
type DifficultyProvider interface {
	// GetTargetHash returns the target hash for mining difficulty for a service
	// at the given session start height.
	// Returns the base difficulty (all relays applicable) if service not found.
	GetTargetHash(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error)
}

// ServiceComputeUnitsProvider provides compute units per relay for services.
type ServiceComputeUnitsProvider interface {
	// GetServiceComputeUnits returns the compute units per relay for a service.
	GetServiceComputeUnits(serviceID string) uint64
}

// RelaySignerKeyring provides relay signing capabilities.
type RelaySignerKeyring interface {
	// SignRelayResponse signs the relay response with the supplier's key.
	SignRelayResponse(
		ctx context.Context,
		response *servicetypes.RelayResponse,
		supplierOperatorAddr string,
	) ([]byte, error)
}

// AppDiscoveryCallback is called when a new application is discovered during relay processing.
type AppDiscoveryCallback func(ctx context.Context, appAddress string)

// relayProcessor implements RelayProcessor.
type relayProcessor struct {
	logger                      logging.Logger
	publisher                   transport.MinedRelayPublisher
	signer                      RelaySignerKeyring
	difficultyProvider          DifficultyProvider
	serviceComputeUnitsProvider ServiceComputeUnitsProvider
	ringClient                  crypto.RingClient

	// Optional callback for discovered apps (e.g., to persist them in CacheWarmer)
	appDiscoveryCallback AppDiscoveryCallback

	mu sync.RWMutex
}

// NewRelayProcessor creates a new relay processor.
func NewRelayProcessor(
	logger logging.Logger,
	publisher transport.MinedRelayPublisher,
	signer RelaySignerKeyring,
	ringClient crypto.RingClient,
) *relayProcessor {
	return &relayProcessor{
		logger:     logging.ForComponent(logger, logging.ComponentRelayProcessor),
		publisher:  publisher,
		signer:     signer,
		ringClient: ringClient,
	}
}

// SetDifficultyProvider sets the difficulty provider.
func (rp *relayProcessor) SetDifficultyProvider(provider DifficultyProvider) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.difficultyProvider = provider
}

// SetServiceComputeUnitsProvider sets the service compute units provider.
func (rp *relayProcessor) SetServiceComputeUnitsProvider(provider ServiceComputeUnitsProvider) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.serviceComputeUnitsProvider = provider
}

// SetAppDiscoveryCallback sets the callback for discovered applications.
// This is used to persist newly discovered apps for cache warming.
func (rp *relayProcessor) SetAppDiscoveryCallback(callback AppDiscoveryCallback) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.appDiscoveryCallback = callback
}

// ProcessRelay processes a served relay.
func (rp *relayProcessor) ProcessRelay(
	ctx context.Context,
	reqBody, respBody []byte,
	supplierAddr string,
	serviceID string,
	arrivalBlockHeight int64,
) (*transport.MinedRelayMessage, error) {
	// Try to deserialize as a protobuf RelayRequest
	relayReq := &servicetypes.RelayRequest{}
	if err := relayReq.Unmarshal(reqBody); err != nil {
		// Not a valid relay request - this is common for non-relay traffic
		// that passes through the proxy. Just skip processing.
		rp.logger.Debug().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("request body is not a valid RelayRequest, skipping relay processing")
		return nil, nil
	}

	// Build relay response
	relayResp, err := rp.buildRelayResponse(ctx, relayReq, respBody, supplierAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to build relay response: %w", err)
	}

	// Create the full relay
	relay := &servicetypes.Relay{
		Req: relayReq,
		Res: relayResp,
	}

	// CRITICAL: Dehydrate the payload BEFORE marshaling for SMST storage.
	// This ensures the SMST root hash matches what the blockchain validator expects.
	// The signature was already computed over the full payload in buildRelayResponse(),
	// and the PayloadHash field allows verification without the full payload (v0.1.25+).
	// Reference: ../poktroll/pkg/relayer/miner/miner.go:107-111
	relayResp.Payload = nil

	// Calculate relay hash (now WITHOUT payload - matches blockchain expectation)
	relayBz, err := relay.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal relay: %w", err)
	}

	relayHash := protocol.GetRelayHashFromBytes(relayBz)

	// Extract session info from relay request for logging and message construction
	sessionHeader := relayReq.Meta.SessionHeader
	sessionID := ""
	sessionStartHeight := int64(0)
	sessionEndHeight := int64(0)
	appAddress := ""

	if sessionHeader != nil {
		sessionID = sessionHeader.SessionId
		sessionStartHeight = sessionHeader.SessionStartBlockHeight
		sessionEndHeight = sessionHeader.SessionEndBlockHeight
		appAddress = sessionHeader.ApplicationAddress
	}

	// Check mining difficulty using the difficulty at session start height
	isApplicable, err := rp.checkMiningDifficulty(ctx, serviceID, relayHash[:], sessionStartHeight)
	if err != nil {
		rp.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to check mining difficulty, assuming applicable")
		isApplicable = true // Default to applicable on error
	}

	if !isApplicable {
		// Relay doesn't meet difficulty, skip publishing
		sessionCtx := logging.SessionContextPartial(sessionID, serviceID, supplierAddr, appAddress, sessionEndHeight)
		logging.WithSessionContext(rp.logger.Debug(), sessionCtx).
			Msg("relay does not meet mining difficulty, skipping")
		relaysSkippedDifficulty.WithLabelValues(serviceID).Inc()
		return nil, nil
	}

	if sessionHeader != nil {
		appAddress = sessionHeader.ApplicationAddress

		// Notify about discovered app for cache warming persistence
		if appAddress != "" {
			rp.mu.RLock()
			callback := rp.appDiscoveryCallback
			rp.mu.RUnlock()

			if callback != nil {
				// Call asynchronously to avoid blocking relay processing
				go callback(ctx, appAddress)
			}
		}
	}

	// Build mined relay message
	msg := &transport.MinedRelayMessage{
		RelayHash:               relayHash[:],
		RelayBytes:              relayBz,
		ComputeUnitsPerRelay:    rp.getComputeUnits(serviceID),
		SessionId:               sessionID,
		SessionStartHeight:      sessionStartHeight,
		SessionEndHeight:        sessionEndHeight,
		SupplierOperatorAddress: supplierAddr,
		ServiceId:               serviceID,
		ApplicationAddress:      appAddress,
		ArrivalBlockHeight:      arrivalBlockHeight,
	}
	msg.SetPublishedAt()

	return msg, nil
}

// buildRelayResponse creates and signs a relay response.
func (rp *relayProcessor) buildRelayResponse(
	ctx context.Context,
	relayReq *servicetypes.RelayRequest,
	respBody []byte,
	supplierAddr string,
) (*servicetypes.RelayResponse, error) {
	// Create relay response with the backend response payload
	relayResp := &servicetypes.RelayResponse{
		Meta: servicetypes.RelayResponseMetadata{
			SessionHeader: relayReq.Meta.SessionHeader,
		},
		Payload: respBody,
	}

	// Calculate payload hash for signature efficiency (v0.1.25+)
	// This allows the response payload to be nil'd before SMST storage
	// while still being verifiable via the payload hash
	payloadHash := sha256.Sum256(respBody)
	relayResp.PayloadHash = payloadHash[:]

	// Sign the response if signer is available
	if rp.signer != nil {
		sig, err := rp.signer.SignRelayResponse(ctx, relayResp, supplierAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to sign relay response: %w", err)
		}
		relayResp.Meta.SupplierOperatorSignature = sig
	}

	// NOTE: Payload is NOT cleared here - it's cleared in ProcessRelay() BEFORE marshaling.
	// This ensures the relay bytes stored in the SMST match what the blockchain expects.
	return relayResp, nil
}

// checkMiningDifficulty checks if a relay hash meets the mining difficulty at session start.
func (rp *relayProcessor) checkMiningDifficulty(
	ctx context.Context,
	serviceID string,
	relayHash []byte,
	sessionStartHeight int64,
) (bool, error) {
	targetHash, err := rp.GetServiceDifficulty(ctx, serviceID, sessionStartHeight)
	if err != nil {
		return false, err
	}

	return protocol.IsRelayVolumeApplicable(relayHash, targetHash), nil
}

// GetServiceDifficulty returns the mining difficulty for a service at the given session start height.
func (rp *relayProcessor) GetServiceDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error) {
	rp.mu.RLock()
	provider := rp.difficultyProvider
	rp.mu.RUnlock()

	if provider == nil {
		// No provider, use base difficulty (all relays applicable)
		return protocol.BaseRelayDifficultyHashBz, nil
	}

	return provider.GetTargetHash(ctx, serviceID, sessionStartHeight)
}

// getComputeUnits returns the compute units per relay for a service.
// Uses the configured provider, or falls back to 1 if not available.
func (rp *relayProcessor) getComputeUnits(serviceID string) uint64 {
	rp.mu.RLock()
	provider := rp.serviceComputeUnitsProvider
	rp.mu.RUnlock()

	if provider == nil {
		// No provider, fall back to 1 (will likely cause claim failures)
		return 1
	}

	return provider.GetServiceComputeUnits(serviceID)
}

// BaseDifficultyProvider always returns the base difficulty (all relays applicable).
// Useful for testing or when on-chain difficulty queries are not available.
type BaseDifficultyProvider struct{}

// GetTargetHash returns the base difficulty hash (sessionStartHeight is ignored).
func (p *BaseDifficultyProvider) GetTargetHash(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error) {
	return protocol.BaseRelayDifficultyHashBz, nil
}

// ServiceDifficultyQueryClient queries on-chain service difficulty at a specific height.
type ServiceDifficultyQueryClient interface {
	// GetServiceRelayDifficulty returns the relay mining difficulty for a service at a given session start height.
	GetServiceRelayDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error)
}

// QueryDifficultyProvider is a thin pass-through to the query layer for difficulty lookups.
// Caching of successful responses is handled by the query layer
// (serviceQueryClient.heightDifficultyCache) — no duplicate caching here.
type QueryDifficultyProvider struct {
	logger      logging.Logger
	queryClient ServiceDifficultyQueryClient
}

// NewQueryDifficultyProvider creates a new query-based difficulty provider.
func NewQueryDifficultyProvider(
	logger logging.Logger,
	queryClient ServiceDifficultyQueryClient,
) *QueryDifficultyProvider {
	return &QueryDifficultyProvider{
		logger:      logging.ForComponent(logger, logging.ComponentDifficultyProvider),
		queryClient: queryClient,
	}
}

// GetTargetHash returns the difficulty target for a service at the given session start height.
// Returns base difficulty (all relays applicable) if sessionStartHeight <= 0 (e.g., nil sessionHeader).
func (p *QueryDifficultyProvider) GetTargetHash(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error) {
	// Guard: invalid height (e.g., nil sessionHeader → height 0) returns base difficulty
	// to avoid querying with a meaningless height.
	if sessionStartHeight <= 0 {
		return protocol.BaseRelayDifficultyHashBz, nil
	}

	if p.queryClient == nil {
		return protocol.BaseRelayDifficultyHashBz, nil
	}

	target, err := p.queryClient.GetServiceRelayDifficulty(ctx, serviceID, sessionStartHeight)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Int64("session_start_height", sessionStartHeight).
			Msg("failed to query service difficulty, using base")
		return protocol.BaseRelayDifficultyHashBz, nil
	}

	return target, nil
}

// =============================================================================
// Cached Service Compute Units Provider
// =============================================================================

// ServiceQueryClient queries on-chain service data.
type ServiceQueryClient interface {
	// GetService returns the service entity for a service ID.
	GetService(ctx context.Context, serviceID string) (sharedtypes.Service, error)
}

// CachedServiceComputeUnitsProvider caches compute units per service from on-chain data.
type CachedServiceComputeUnitsProvider struct {
	logger      logging.Logger
	queryClient ServiceQueryClient

	cache sync.Map // map[serviceID]uint64
}

// NewCachedServiceComputeUnitsProvider creates a new cached compute units provider.
func NewCachedServiceComputeUnitsProvider(
	logger logging.Logger,
	queryClient ServiceQueryClient,
) *CachedServiceComputeUnitsProvider {
	return &CachedServiceComputeUnitsProvider{
		logger:      logging.ForComponent(logger, logging.ComponentRelayProcessor),
		queryClient: queryClient,
	}
}

// GetServiceComputeUnits returns the compute units per relay for a service.
// If the service is not cached, it queries the chain synchronously.
func (p *CachedServiceComputeUnitsProvider) GetServiceComputeUnits(serviceID string) uint64 {
	// Check cache first
	if cached, ok := p.cache.Load(serviceID); ok {
		return cached.(uint64)
	}

	// Query on-chain (synchronously - consider background refresh for production)
	if p.queryClient == nil {
		p.logger.Warn().
			Str(logging.FieldServiceID, serviceID).
			Msg("no query client available, using default compute units")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	service, err := p.queryClient.GetService(ctx, serviceID)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to query service compute units, using default")
		return 1
	}

	computeUnits := service.ComputeUnitsPerRelay
	p.cache.Store(serviceID, computeUnits)

	p.logger.Debug().
		Str(logging.FieldServiceID, serviceID).
		Uint64("compute_units", computeUnits).
		Msg("cached service compute units from chain")

	return computeUnits
}

// PreloadServiceComputeUnits preloads compute units for a list of services.
// Call this at startup to avoid synchronous queries during relay processing.
func (p *CachedServiceComputeUnitsProvider) PreloadServiceComputeUnits(ctx context.Context, serviceIDs []string) {
	for _, serviceID := range serviceIDs {
		if p.queryClient == nil {
			continue
		}

		service, err := p.queryClient.GetService(ctx, serviceID)
		if err != nil {
			p.logger.Warn().
				Err(err).
				Str(logging.FieldServiceID, serviceID).
				Msg("failed to preload service compute units")
			continue
		}

		p.cache.Store(serviceID, service.ComputeUnitsPerRelay)
		p.logger.Info().
			Str(logging.FieldServiceID, serviceID).
			Uint64("compute_units", service.ComputeUnitsPerRelay).
			Msg("preloaded service compute units")
	}
}

// InvalidateCache clears the compute units cache for a service.
func (p *CachedServiceComputeUnitsProvider) InvalidateCache(serviceID string) {
	p.cache.Delete(serviceID)
}

// InvalidateAllCache clears all cached compute units.
func (p *CachedServiceComputeUnitsProvider) InvalidateAllCache() {
	p.cache.Range(func(key, value interface{}) bool {
		p.cache.Delete(key)
		return true
	})
}

// Verify interface compliance.
var _ RelayProcessor = (*relayProcessor)(nil)
var _ DifficultyProvider = (*BaseDifficultyProvider)(nil)
var _ DifficultyProvider = (*QueryDifficultyProvider)(nil)
var _ ServiceComputeUnitsProvider = (*CachedServiceComputeUnitsProvider)(nil)
