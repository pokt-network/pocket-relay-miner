package relayer

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	"github.com/pokt-network/poktroll/pkg/crypto"
	"github.com/pokt-network/poktroll/pkg/crypto/protocol"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
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

	// ProcessRelayPayloadHash is ProcessRelay given the sha256 of the response
	// body instead of the body: the mined relay keeps only that hash.
	ProcessRelayPayloadHash(
		ctx context.Context,
		reqBody []byte,
		respPayloadHash [32]byte,
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
	// GetServiceComputeUnits returns the compute units per relay that was effective
	// for a service at sessionStartHeight.
	//
	// The height is NOT optional. This value becomes the SMST leaf weight, and from
	// poktroll v0.1.35 the chain resolves CUPR at session start when it validates the
	// claim (x/proof) and again at settlement (x/tokenomics). Reading the LIVE value
	// instead lets a mid-session CUPR change split one session's tree into two
	// weights, so smstSum != numRelays * cupr and MsgCreateClaim is rejected with
	// ErrProofComputeUnitsMismatch — forfeiting the session.
	GetServiceComputeUnits(ctx context.Context, serviceID string, sessionStartHeight int64) uint64
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

// relayProcessor implements RelayProcessor.
type relayProcessor struct {
	logger                      logging.Logger
	publisher                   transport.MinedRelayPublisher
	signer                      RelaySignerKeyring
	difficultyProvider          DifficultyProvider
	serviceComputeUnitsProvider ServiceComputeUnitsProvider
	ringClient                  crypto.RingClient

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

// ProcessRelay processes a served relay.
func (rp *relayProcessor) ProcessRelay(
	ctx context.Context,
	reqBody, respBody []byte,
	supplierAddr string,
	serviceID string,
	arrivalBlockHeight int64,
) (*transport.MinedRelayMessage, error) {
	return rp.ProcessRelayPayloadHash(ctx, reqBody, sha256.Sum256(respBody), supplierAddr, serviceID, arrivalBlockHeight)
}

// ProcessRelayPayloadHash implements RelayProcessor.
func (rp *relayProcessor) ProcessRelayPayloadHash(
	ctx context.Context,
	reqBody []byte,
	respPayloadHash [32]byte,
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
	relayResp, err := rp.buildRelayResponse(ctx, relayReq, respPayloadHash, supplierAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to build relay response: %w", err)
	}

	// Create the full relay
	relay := &servicetypes.Relay{
		Req: relayReq,
		Res: relayResp,
	}

	// The relay carries no payload, only its hash: the SMST root hash then
	// matches what the blockchain validator expects, and the PayloadHash field
	// allows verification without the payload (v0.1.25+).
	// Reference: ../poktroll/pkg/relayer/miner/miner.go:107-111

	// Calculate relay hash (WITHOUT payload - matches blockchain expectation)
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
		// Served, not mined. Mined against another target, a relay below the
		// real one could be the leaf the chain picks for the proof, which it
		// validates against the difficulty at the session start: the whole
		// session's proof would fail. Debug is per-request, so the counter is
		// the only signal that difficulty is unresolvable.
		difficultyQueryFailures.WithLabelValues(serviceID).Inc()
		rp.logger.Debug().
			Err(err).
			Str(logging.FieldServiceID, serviceID).
			Msg("failed to resolve mining difficulty, relay served and not mined")
		return nil, nil
	}

	if !isApplicable {
		// Relay doesn't meet difficulty, skip publishing
		sessionCtx := logging.SessionContextPartial(sessionID, serviceID, supplierAddr, appAddress, sessionEndHeight)
		logging.WithSessionContext(rp.logger.Debug(), sessionCtx).
			Msg("relay does not meet mining difficulty, skipping")
		relaysSkippedDifficulty.WithLabelValues(serviceID, RPCTypeFrom(ctx)).Inc()
		return nil, nil
	}

	// Build mined relay message
	msg := &transport.MinedRelayMessage{
		RelayHash:               relayHash[:],
		RelayBytes:              relayBz,
		ComputeUnitsPerRelay:    rp.getComputeUnits(ctx, serviceID, sessionStartHeight),
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

// emptyPayloadHash is sha256 of an empty response body.
var emptyPayloadHash = sha256.Sum256(nil)

// errEmptyResponsePayload refuses to mine a relay whose response body is empty.
var errEmptyResponsePayload = errors.New("attempted to sign a relay response with an empty payload")

// buildRelayResponse creates and signs a relay response from the hash of the
// backend response body. The payload itself is left out: the signature covers
// the payload hash and not the payload (RelayResponse.GetSignableBytesHash drops
// the payload once the hash is set), and the payload is dropped before the
// relay reaches the tree anyway.
func (rp *relayProcessor) buildRelayResponse(
	ctx context.Context,
	relayReq *servicetypes.RelayRequest,
	respPayloadHash [32]byte,
	supplierAddr string,
) (*servicetypes.RelayResponse, error) {
	// An empty response is not mined, as when the payload itself was signed
	// (RelayResponse.UpdatePayloadHash refuses an empty payload).
	if respPayloadHash == emptyPayloadHash {
		return nil, errEmptyResponsePayload
	}
	relayResp := &servicetypes.RelayResponse{
		Meta: servicetypes.RelayResponseMetadata{
			SessionHeader: relayReq.Meta.SessionHeader,
		},
		PayloadHash: respPayloadHash[:],
	}

	// Sign the response if signer is available
	if rp.signer != nil {
		sig, err := rp.signer.SignRelayResponse(ctx, relayResp, supplierAddr)
		if err != nil {
			return nil, fmt.Errorf("failed to sign relay response: %w", err)
		}
		relayResp.Meta.SupplierOperatorSignature = sig
	}

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

// getComputeUnits returns the compute units per relay that was effective for a
// service at sessionStartHeight — the value the chain validates the claim against.
// Uses the configured provider, or falls back to 1 if not available.
func (rp *relayProcessor) getComputeUnits(ctx context.Context, serviceID string, sessionStartHeight int64) uint64 {
	rp.mu.RLock()
	provider := rp.serviceComputeUnitsProvider
	rp.mu.RUnlock()

	if provider == nil {
		// No provider, fall back to 1 (will likely cause claim failures)
		return 1
	}

	return provider.GetServiceComputeUnits(ctx, serviceID, sessionStartHeight)
}

// ServiceDifficultyQueryClient queries on-chain service difficulty at a specific height.
type ServiceDifficultyQueryClient interface {
	// GetServiceRelayDifficulty returns the relay mining difficulty for a service at a given session start height.
	GetServiceRelayDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error)
}

// QueryDifficultyProvider is a thin pass-through to the query layer for difficulty lookups.
// Caching of successful responses is handled by the query layer
// (serviceQueryClient.difficultyAtHeight) — no duplicate caching here.
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
		// Returned, not replaced by the base target: the processor does not
		// mine a relay whose difficulty it could not resolve.
		return nil, fmt.Errorf("difficulty of %s at height %d: %w", serviceID, sessionStartHeight, err)
	}

	return target, nil
}

// Verify interface compliance.
var _ RelayProcessor = (*relayProcessor)(nil)
var _ DifficultyProvider = (*QueryDifficultyProvider)(nil)
var _ ServiceComputeUnitsProvider = (*serviceCacheComputeUnitsProvider)(nil)
