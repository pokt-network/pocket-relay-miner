package logging

import (
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	"github.com/rs/zerolog"
)

// SessionContext contains relay session context fields for structured logging.
type SessionContext struct {
	SessionID        string
	ServiceID        string
	Supplier         string
	Application      string
	SessionEndHeight int64
}

// WithSessionContext adds all session context fields to a log event.
// Use this helper to ensure consistent session logging across all relayer components.
func WithSessionContext(event *zerolog.Event, ctx *SessionContext) *zerolog.Event {
	if ctx == nil {
		return event
	}

	if ctx.SessionID != "" {
		event = event.Str(FieldSessionID, ctx.SessionID)
	}
	if ctx.ServiceID != "" {
		event = event.Str(FieldServiceID, ctx.ServiceID)
	}
	if ctx.Supplier != "" {
		event = event.Str(FieldSupplier, ctx.Supplier)
	}
	if ctx.Application != "" {
		event = event.Str(FieldApplication, ctx.Application)
	}
	if ctx.SessionEndHeight > 0 {
		event = event.Int64(FieldSessionEndHeight, ctx.SessionEndHeight)
	}

	return event
}

// SessionContextFromRelayRequest extracts session context from a RelayRequest.
// This is the canonical way to get structured logging context for relay operations.
func SessionContextFromRelayRequest(req *servicetypes.RelayRequest) *SessionContext {
	if req == nil || req.Meta.SessionHeader == nil {
		return &SessionContext{}
	}

	header := req.Meta.SessionHeader
	return &SessionContext{
		SessionID:        header.SessionId,
		ServiceID:        header.ServiceId,
		Application:      header.ApplicationAddress,
		SessionEndHeight: header.SessionEndBlockHeight,
		Supplier:         req.Meta.SupplierOperatorAddress,
	}
}

// SessionContextPartial creates a partial session context from individual fields.
// Use this when you don't have a full RelayRequest but have the individual fields.
func SessionContextPartial(sessionID, serviceID, supplier, application string, sessionEndHeight int64) *SessionContext {
	return &SessionContext{
		SessionID:        sessionID,
		ServiceID:        serviceID,
		Supplier:         supplier,
		Application:      application,
		SessionEndHeight: sessionEndHeight,
	}
}
