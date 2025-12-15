package relayer

import (
	"context"
	"fmt"

	servicev1 "github.com/pokt-network/poktroll/x/service/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// RelayPipeline provides a unified processing pipeline for all relay protocols.
// It consolidates validation, metering, signing, and publishing logic to ensure
// consistent behavior across HTTP, WebSocket, gRPC, and Streaming transports.
//
// This is the single source of truth for relay processing.
type RelayPipeline struct {
	validator      RelayValidator
	relayMeter     *RelayMeter
	responseSigner *ResponseSigner
	relayProcessor RelayProcessor
	logger         logging.Logger
	metricRecorder *MetricRecorder
	config         *Config
}

// NewRelayPipeline creates a new relay processing pipeline.
func NewRelayPipeline(
	validator RelayValidator,
	relayMeter *RelayMeter,
	responseSigner *ResponseSigner,
	relayProcessor RelayProcessor,
	logger logging.Logger,
	metricRecorder *MetricRecorder,
	config *Config,
) *RelayPipeline {
	return &RelayPipeline{
		validator:      validator,
		relayMeter:     relayMeter,
		responseSigner: responseSigner,
		relayProcessor: relayProcessor,
		logger:         logging.ForComponent(logger, "relay_pipeline"),
		metricRecorder: metricRecorder,
		config:         config,
	}
}

// RelayContext contains all information needed to process a relay.
type RelayContext struct {
	// Request is the relay request from the gateway client
	Request *servicev1.RelayRequest

	// Response is the relay response from the backend (to be signed)
	Response *servicev1.RelayResponse

	// ServiceID is the service identifier
	ServiceID string

	// SupplierAddress is the supplier's operator address
	SupplierAddress string

	// SessionID is the session identifier
	SessionID string

	// Payload is the backend response payload
	Payload []byte

	// ComputeUnits is the compute units for this relay
	ComputeUnits uint64

	// ArrivalBlockHeight is the block height when the relay arrived
	ArrivalBlockHeight int64
}

// ProcessingResult contains the outcome of relay processing.
type ProcessingResult struct {
	// Allowed indicates if the relay passed validation and metering checks
	Allowed bool

	// OverServiced indicates if the relay exceeded application stake limits
	// (relay served but won't be mined)
	OverServiced bool

	// ValidationErr contains validation error (signature/session)
	ValidationErr error

	// MeteringErr contains metering error (stake check)
	MeteringErr error

	// SigningErr contains response signing error
	SigningErr error

	// PublishingErr contains publishing error (Redis)
	PublishingErr error

	// SignedResponse is the signed relay response (if signing succeeded)
	SignedResponse *servicev1.RelayResponse
}

// ProcessingMode determines the relay processing behavior.
type ProcessingMode int

const (
	// ModeEager validates and meters BEFORE serving the relay
	ModeEager ProcessingMode = iota

	// ModeOptimistic serves the relay first, validates/meters in background
	ModeOptimistic
)

// ValidateRelay validates the relay request (ring signature + session).
func (p *RelayPipeline) ValidateRelay(
	ctx context.Context,
	relayCtx *RelayContext,
) error {
	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Msg("validating relay request")

	// Validate relay request (ring signature + session)
	if err := p.validator.ValidateRelayRequest(ctx, relayCtx.Request); err != nil {
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Str("supplier", relayCtx.SupplierAddress).
			Msg("relay validation failed")
		return fmt.Errorf("validation failed: %w", err)
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay validation passed")
	return nil
}

// MeterRelay checks and consumes relay stake (rate limiting).
// Returns (allowed, overServiced, error).
func (p *RelayPipeline) MeterRelay(
	ctx context.Context,
	relayCtx *RelayContext,
) (bool, bool, error) {
	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Msg("metering relay")

	// Extract session information for meter check
	sessionHeader := relayCtx.Request.Meta.SessionHeader
	sessionID := sessionHeader.SessionId
	appAddress := sessionHeader.ApplicationAddress

	// Check and consume relay stake
	allowed, overServiced, err := p.relayMeter.CheckAndConsumeRelay(
		ctx,
		sessionID,
		appAddress,
		relayCtx.ServiceID,
		int64(relayCtx.ComputeUnits),
	)

	if err != nil {
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay metering failed")
		return false, false, fmt.Errorf("metering failed: %w", err)
	}

	if overServiced {
		p.logger.Warn().
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay over-serviced (will be served but not mined)")
	}

	if !allowed {
		p.logger.Warn().
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay not allowed (stake limit exceeded)")
	}

	return allowed, overServiced, nil
}

// SignResponse signs the relay response with the supplier's private key.
func (p *RelayPipeline) SignResponse(
	ctx context.Context,
	relayCtx *RelayContext,
) (*servicev1.RelayResponse, error) {
	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Msg("signing relay response")

	// Sign the response
	if err := p.responseSigner.SignRelayResponse(
		relayCtx.Response,
		relayCtx.SupplierAddress,
	); err != nil {
		p.logger.Error().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Str("supplier", relayCtx.SupplierAddress).
			Msg("failed to sign relay response")
		return nil, fmt.Errorf("signing failed: %w", err)
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay response signed successfully")
	return relayCtx.Response, nil
}

// PublishRelay publishes the relay to Redis Streams for mining.
// Only publishes if the relay is allowed and not over-serviced.
func (p *RelayPipeline) PublishRelay(
	ctx context.Context,
	relayCtx *RelayContext,
	result *ProcessingResult,
) error {
	// Don't publish if not allowed or over-serviced
	if !result.Allowed || result.OverServiced {
		p.logger.Debug().
			Bool("allowed", result.Allowed).
			Bool("over_serviced", result.OverServiced).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("skipping relay publishing (not allowed or over-serviced)")
		return nil
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("publishing relay to Redis")

	// Serialize relay request
	relayRequestBz, err := relayCtx.Request.Marshal()
	if err != nil {
		p.logger.Error().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("failed to marshal relay request")
		return fmt.Errorf("marshal request failed: %w", err)
	}

	// Process relay (hashing, difficulty check, mining)
	_, err = p.relayProcessor.ProcessRelay(
		ctx,
		relayRequestBz,
		relayCtx.Payload,
		relayCtx.SupplierAddress,
		relayCtx.ServiceID,
		relayCtx.ArrivalBlockHeight,
	)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay processing failed (may not meet difficulty)")
		return fmt.Errorf("processing failed: %w", err)
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay published successfully")
	return nil
}

// ProcessRelayEager processes a relay in eager mode:
// 1. Validate (ring signature + session)
// 2. Meter (check stake before serving)
// 3. Backend call happens OUTSIDE this function
// 4. Sign response (after backend call)
// 5. Publish to Redis (async)
//
// Returns ProcessingResult with validation/metering outcomes.
func (p *RelayPipeline) ProcessRelayEager(
	ctx context.Context,
	relayCtx *RelayContext,
) *ProcessingResult {
	result := &ProcessingResult{
		Allowed:      true,
		OverServiced: false,
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Str("mode", "eager").
		Msg("processing relay (eager mode)")

	// Step 1: Validate relay request
	if err := p.ValidateRelay(ctx, relayCtx); err != nil {
		result.Allowed = false
		result.ValidationErr = err
		validationFailures.WithLabelValues(relayCtx.ServiceID, "signature_or_session").Inc()
		return result
	}

	// Step 2: Meter relay (check stake)
	allowed, overServiced, err := p.MeterRelay(ctx, relayCtx)
	if err != nil {
		result.MeteringErr = err
		// Fail-open: allow relay on metering error (log error)
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("metering error (fail-open: allowing relay)")
	} else {
		result.Allowed = allowed
		result.OverServiced = overServiced

		if !allowed {
			relaysRejected.WithLabelValues(relayCtx.ServiceID, "stake_limit_exceeded").Inc()
			return result
		}
	}

	// Step 3: Backend call happens in caller
	// Step 4: Signing happens in SignAndPublishResponse()

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay validation and metering complete (eager mode)")
	return result
}

// SignAndPublishResponse signs the response and publishes the relay.
// This should be called AFTER the backend response is received.
// Publishing happens asynchronously via worker pool.
func (p *RelayPipeline) SignAndPublishResponse(
	ctx context.Context,
	relayCtx *RelayContext,
	result *ProcessingResult,
) (*servicev1.RelayResponse, error) {
	// Step 1: Sign response
	signedResponse, err := p.SignResponse(ctx, relayCtx)
	if err != nil {
		result.SigningErr = err
		relaysRejected.WithLabelValues(relayCtx.ServiceID, "signing_failed").Inc()
		return nil, err
	}

	result.SignedResponse = signedResponse

	// Step 2: Publish relay (async via worker pool if available)
	// Note: We don't return the error here as publishing is best-effort
	if err := p.PublishRelay(ctx, relayCtx, result); err != nil {
		result.PublishingErr = err
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay publishing failed (best-effort)")
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay response signed and published")
	return signedResponse, nil
}

// ProcessRelayOptimistic processes a relay in optimistic mode:
// 1. Backend call happens OUTSIDE this function
// 2. Sign response
// 3. Validate (async, after response sent)
// 4. Meter (async, after response sent)
// 5. Publish (async, only if validation/metering passed)
//
// Returns signed response immediately (validation/metering happen in background).
func (p *RelayPipeline) ProcessRelayOptimistic(
	ctx context.Context,
	relayCtx *RelayContext,
) (*servicev1.RelayResponse, *ProcessingResult) {
	result := &ProcessingResult{
		Allowed:      true,
		OverServiced: false,
	}

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Str("mode", "optimistic").
		Msg("processing relay (optimistic mode)")

	// Step 1: Backend call happens in caller
	// Step 2: Sign response immediately
	signedResponse, err := p.SignResponse(ctx, relayCtx)
	if err != nil {
		result.SigningErr = err
		result.Allowed = false
		relaysRejected.WithLabelValues(relayCtx.ServiceID, "signing_failed").Inc()
		return nil, result
	}

	result.SignedResponse = signedResponse

	// Step 3 & 4: Validation and metering happen in background (caller's responsibility)
	// The caller should spawn goroutine to call ValidateAndMeterAsync()

	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("relay response signed (validation/metering will happen async)")
	return signedResponse, result
}

// ValidateAndMeterAsync performs validation and metering asynchronously.
// Should be called in a goroutine after serving the relay in optimistic mode.
// Returns updated ProcessingResult.
func (p *RelayPipeline) ValidateAndMeterAsync(
	ctx context.Context,
	relayCtx *RelayContext,
	result *ProcessingResult,
) *ProcessingResult {
	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Str("mode", "optimistic_async").
		Msg("async validation and metering started")

	// Step 1: Validate
	if err := p.ValidateRelay(ctx, relayCtx); err != nil {
		result.Allowed = false
		result.ValidationErr = err
		validationFailures.WithLabelValues(relayCtx.ServiceID, "signature_or_session").Inc()
		relaysDropped.WithLabelValues(relayCtx.ServiceID, "validation_failed").Inc()
		return result
	}

	// Step 2: Meter
	allowed, overServiced, err := p.MeterRelay(ctx, relayCtx)
	if err != nil {
		result.MeteringErr = err
		// Fail-open: allow relay on metering error
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("metering error (fail-open: allowing relay)")
	} else {
		result.Allowed = allowed
		result.OverServiced = overServiced

		if !allowed {
			relaysDropped.WithLabelValues(relayCtx.ServiceID, "meter_rejected").Inc()
			return result
		}
	}

	// Step 3: Publish if allowed and not over-serviced
	if err := p.PublishRelay(ctx, relayCtx, result); err != nil {
		result.PublishingErr = err
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay publishing failed (best-effort)")
	}

	p.logger.Debug().
		Bool("allowed", result.Allowed).
		Bool("over_serviced", result.OverServiced).
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Msg("async validation and metering complete")

	return result
}
