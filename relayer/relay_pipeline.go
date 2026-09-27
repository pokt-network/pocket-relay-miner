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
	validator  RelayValidator
	relayMeter *RelayMeter
	logger     logging.Logger
}

// NewRelayPipeline creates a new relay processing pipeline.
func NewRelayPipeline(
	validator RelayValidator,
	relayMeter *RelayMeter,
	logger logging.Logger,
) *RelayPipeline {
	return &RelayPipeline{
		validator:  validator,
		relayMeter: relayMeter,
		logger:     logging.ForComponent(logger, logging.ComponentRelayPipeline),
	}
}

// RelayContext contains all information needed to process a relay.
type RelayContext struct {
	// Request is the relay request from the gateway client
	Request *servicev1.RelayRequest

	// ServiceID is the service identifier
	ServiceID string

	// SupplierAddress is the supplier's operator address
	SupplierAddress string

	// SessionID is the session identifier
	SessionID string

	// ArrivalBlockHeight is the block height when the relay arrived
	ArrivalBlockHeight int64
}

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
// Returns (allowed, error).
func (p *RelayPipeline) MeterRelay(
	ctx context.Context,
	relayCtx *RelayContext,
) (bool, error) {
	p.logger.Debug().
		Str("service_id", relayCtx.ServiceID).
		Str("session_id", relayCtx.SessionID).
		Str("supplier", relayCtx.SupplierAddress).
		Msg("metering relay")

	// Extract session information for meter check
	sessionHeader := relayCtx.Request.Meta.SessionHeader
	sessionID := sessionHeader.SessionId
	appAddress := sessionHeader.ApplicationAddress
	supplierAddress := relayCtx.SupplierAddress
	sessionStartHeight := sessionHeader.SessionStartBlockHeight
	sessionEndHeight := sessionHeader.SessionEndBlockHeight

	// Check and consume relay stake
	allowed, err := p.relayMeter.CheckAndConsumeRelay(
		ctx,
		sessionID,
		appAddress,
		relayCtx.ServiceID,
		supplierAddress,
		sessionStartHeight,
		sessionEndHeight,
		relayCtx.ArrivalBlockHeight,
	)
	if err != nil {
		p.logger.Warn().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay metering failed")
		return false, fmt.Errorf("metering failed: %w", err)
	}

	if !allowed {
		p.logger.Warn().
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay not allowed (stake limit exceeded)")
	}

	return allowed, nil
}
