package relayer

import (
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
)

// logCircuitBreakerTransition logs a backend circuit-breaker state change.
//
// Warn when a backend goes down, Info when it recovers. These are state
// changes, not per-request events, so they are always visible: an operator
// diagnosing a failover reads exactly these two lines.
//
// The `threshold` argument MUST be the value the breaker actually evaluated --
// the one getCircuitBreakerThreshold returned for this service and RPC type,
// which honours a per-backend health_check.unhealthy_threshold. Both callers
// used to log pool.DefaultUnhealthyThreshold here instead, so an operator who
// raised the threshold to 20 saw "consecutive_failures=20 threshold=5" and had
// no way to tell which number the breaker had used.
func logCircuitBreakerTransition(
	logger logging.Logger,
	transition *pool.TransitionEvent,
	serviceID, rpcType string,
	threshold int32,
) {
	switch {
	case transition.OldHealthy && !transition.NewHealthy:
		// Circuit broken: healthy -> unhealthy.
		event := logger.Warn().
			Str("backend", transition.Endpoint.Name).
			Str("url", transition.Endpoint.RawURL).
			Str(logging.FieldServiceID, serviceID).
			Str("rpc_type", rpcType).
			Int32("consecutive_failures", transition.Failures).
			Int32("threshold", threshold)

		// Classify the triggering cause so operators can tell at a glance *why*
		// the breaker tripped (5xx vs transport error vs DNS vs ...).
		if reason := pool.ClassifyFailure(transition.StatusCode, transition.Error); reason != "" {
			event = event.Str("trigger_reason", reason)
		}
		if transition.StatusCode > 0 {
			event = event.Int("trigger_http_status", transition.StatusCode)
		}
		if transition.Error != nil {
			event = event.Str("trigger_error", transition.Error.Error())
		}
		if recoveryTimeout := transition.Endpoint.RecoveryTimeout(); recoveryTimeout > 0 {
			event = event.Dur("auto_recovery_in", recoveryTimeout)
		}

		event.Msg("BACKEND DOWN: circuit breaker tripped, traffic will failover to other backends")

	case !transition.OldHealthy && transition.NewHealthy:
		// Recovery: unhealthy -> healthy.
		event := logger.Info().
			Str("backend", transition.Endpoint.Name).
			Str("url", transition.Endpoint.RawURL).
			Str(logging.FieldServiceID, serviceID).
			Str("rpc_type", rpcType)

		if transition.DowntimeDuration > 0 {
			event = event.Dur("downtime", transition.DowntimeDuration)
		}
		if transition.StatusCode > 0 {
			event = event.Int("recovery_http_status", transition.StatusCode)
		}

		event.Msg("BACKEND UP: circuit breaker recovered, backend is healthy again")
	}
}
