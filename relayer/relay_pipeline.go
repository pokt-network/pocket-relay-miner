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

// Priced reports whether the pipeline knows what to charge.
//
// The gRPC and WebSocket transports reach the meter only through this pipeline,
// so this is how their admission checks ask the same question the HTTP path asks
// the meter directly.
//
// It is also the ONLY method here that tolerates a nil meter, and that is a
// fact about the code rather than a courtesy: MeterRelay and AdmitRelay both
// dereference p.relayMeter with no guard, and RelayMeter has 37 pointer-receiver methods of
// which none checks m == nil -- admit takes m.mu.RLock() on its first executable
// line. So a pipeline built without a meter does not serve relays free of
// charge: it panics as soon as one reaches that path. A caller passing a nil
// meter is declaring those paths unreachable for itself, which is exactly what
// the session-expired and simulation fixtures state in their own comments.
func (p *RelayPipeline) Priced() bool {
	return p.relayMeter != nil && p.relayMeter.Priced()
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

	// Validate relay request (ring signature + session) at the height THIS relay
	// arrived at.
	//
	// This handover is the whole of the defect it replaced: the arrival height
	// was already in RelayContext and was never passed on, so the two transports
	// that reach the validator ONLY through here -- WebSocket (websocket.go) and
	// gRPC (relay_grpc_service.go) -- had it judge every relay against whatever
	// the last HTTP relay left in a shared field, or against 0, which
	// getTargetSessionBlockHeight reads as "session active". The grace period was
	// therefore never evaluated on either transport: relays long past their grace
	// window validated as live, were served, and were mined into claims the chain
	// does not pay.
	if err := p.validator.ValidateRelayRequest(ctx, relayCtx.Request, relayCtx.ArrivalBlockHeight); err != nil {
		p.logger.Debug().
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

// MeterRelay checks a relay against its budget without reserving or charging
// anything. Returns (allowed, error).
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

	// Check the relay stake, reserving and charging nothing
	allowed, err := p.relayMeter.CheckBudget(
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
		p.logger.Debug().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay metering failed")
		// allowed is PROPAGATED, not flattened to false. The meter answers
		// (false, err) when its own store is unreadable -- admission refuses --
		// and (true, err) when a chain query it depends on blinked, which is
		// served and left for the miner to arbitrate. Returning false here
		// collapsed the two, so every WebSocket and gRPC caller treated a chain
		// blip as a refusal: connections closed, subscriptions dropped.
		return allowed, fmt.Errorf("metering failed: %w", err)
	}

	if !allowed {
		p.logger.Debug().
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay not allowed (stake limit exceeded)")
	}

	return allowed, nil
}

// AdmitRelay reserves a relay's cost before it is served. The caller must
// SettleRelay the reservation once the relay is served and ReleaseRelay it on
// every other exit. Errors carry the same allowed as MeterRelay.
func (p *RelayPipeline) AdmitRelay(
	ctx context.Context,
	relayCtx *RelayContext,
) (Reservation, bool, error) {
	sessionHeader := relayCtx.Request.Meta.SessionHeader
	reservation, allowed, err := p.relayMeter.Admit(
		ctx,
		sessionHeader.SessionId,
		sessionHeader.ApplicationAddress,
		relayCtx.ServiceID,
		relayCtx.SupplierAddress,
		sessionHeader.SessionStartBlockHeight,
		sessionHeader.SessionEndBlockHeight,
		relayCtx.ArrivalBlockHeight,
	)
	if err != nil {
		p.logger.Debug().
			Err(err).
			Str("service_id", relayCtx.ServiceID).
			Str("session_id", relayCtx.SessionID).
			Msg("relay metering failed")
		return reservation, allowed, fmt.Errorf("metering failed: %w", err)
	}
	return reservation, allowed, nil
}

// SettleRelay charges a served relay's reservation.
func (p *RelayPipeline) SettleRelay(reservation Reservation) {
	p.relayMeter.Settle(reservation)
}

// ReleaseRelay gives back the reservation of a relay that was not served.
func (p *RelayPipeline) ReleaseRelay(reservation Reservation) {
	p.relayMeter.Release(reservation)
}

// ChargeServedRelay charges a relay served without an admission of its own and
// reports whether its pair is now at or over the budget.
func (p *RelayPipeline) ChargeServedRelay(
	ctx context.Context,
	sessionID string,
	serviceID string,
	supplierAddress string,
	sessionStartHeight int64,
) (bool, error) {
	return p.relayMeter.ChargeServed(ctx, sessionID, serviceID, supplierAddress, sessionStartHeight)
}

// DispatcherHealthy reports whether a relay served now would be charged, and
// why not when it would not.
func (p *RelayPipeline) DispatcherHealthy() (bool, error) {
	return p.relayMeter.DispatcherHealthy()
}
