package pool

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// tripBreaker drives the endpoint to unhealthy through the pool.
func tripBreaker(t *testing.T, p *Pool, ep *BackendEndpoint) {
	t.Helper()
	var tripped *TransitionEvent
	for i := 0; i < 5; i++ {
		if ev := p.RecordResult(ep, 500, nil, 5); ev != nil {
			tripped = ev
		}
	}
	require.NotNil(t, tripped, "breaker must trip")
	require.False(t, ep.CurrentlyHealthy())
}

// TestAutoRecovery_FirstSuccessEmitsTransitionEvent proves the half-open
// auto-recovery is no longer mute: IsHealthy() flips the state silently (it
// has nowhere to report), so the FIRST recorded success after it must carry
// the unhealthy->healthy TransitionEvent — that event is the only thing that
// ever logs "BACKEND UP".
func TestAutoRecovery_FirstSuccessEmitsTransitionEvent(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	ep.SetRecoveryTimeout(50 * time.Millisecond)
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	tripBreaker(t, p, ep)
	time.Sleep(60 * time.Millisecond)

	// Selection path auto-recovers silently.
	require.True(t, ep.IsHealthy())

	// The first success after auto-recovery must report the transition.
	event := p.RecordResult(ep, 200, nil, 5)
	require.NotNil(t, event, "auto-recovery must be reported on the first success")
	require.False(t, event.OldHealthy)
	require.True(t, event.NewHealthy)
	require.True(t, event.AutoRecovered)
	require.GreaterOrEqual(t, event.DowntimeDuration, 50*time.Millisecond,
		"downtime must survive the silent CAS and cover the whole outage")

	// Exactly once: the next success reports nothing.
	require.Nil(t, p.RecordResult(ep, 200, nil, 5))
}

// TestAutoRecovery_ReTripDropsThePendingRecovery proves that when the backend
// auto-recovers but is still broken, the pending (unreported) UP dies with the
// new DOWN transition: the operator sees DOWN, and the later real recovery is
// reported as a normal transition, never as a stale duplicate.
func TestAutoRecovery_ReTripDropsThePendingRecovery(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	ep.SetRecoveryTimeout(50 * time.Millisecond)
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	tripBreaker(t, p, ep)
	time.Sleep(60 * time.Millisecond)
	require.True(t, ep.IsHealthy(), "auto-recovery must have happened")

	// Still broken: re-trip.
	var down *TransitionEvent
	for i := 0; i < 5; i++ {
		if ev := p.RecordResult(ep, 500, nil, 5); ev != nil {
			down = ev
		}
	}
	require.NotNil(t, down, "re-trip must be reported")
	require.False(t, down.NewHealthy)

	// Real recovery via traffic: exactly one UP event, not auto-recovered.
	up := p.RecordResult(ep, 200, nil, 5)
	require.NotNil(t, up)
	require.True(t, up.NewHealthy)
	require.False(t, up.AutoRecovered,
		"a traffic recovery must not be blamed on the stale auto-recovery")
	require.Nil(t, p.RecordResult(ep, 200, nil, 5), "no duplicate event")
}

// TestCurrentlyHealthy_IsAPureRead proves observers (health checker, status
// API) can read the health flag without triggering the half-open recovery:
// the old code computed wasUnhealthy with IsHealthy(), whose side effect
// swallowed the health checker's own "backend became healthy" log.
func TestCurrentlyHealthy_IsAPureRead(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	ep.SetRecoveryTimeout(50 * time.Millisecond)
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	tripBreaker(t, p, ep)
	time.Sleep(60 * time.Millisecond)

	// Past the recovery timeout: the pure read must NOT auto-recover.
	require.False(t, ep.CurrentlyHealthy())
	require.False(t, ep.CurrentlyHealthy(), "repeated pure reads stay pure")

	// The selection path still does.
	require.True(t, ep.IsHealthy())
	require.True(t, ep.CurrentlyHealthy())
}

// TestAutoRecovery_HealthCheckerRecoveryLeavesNoPendingEvent proves the
// active health checker path (SetHealthy) clears the auto-recovery downtime
// bookkeeping: SetHealthy is only called by code that logs its own
// transition, so the pool must not re-report it later.
func TestAutoRecovery_HealthCheckerRecoveryLeavesNoPendingEvent(t *testing.T) {
	ep, _ := NewBackendEndpoint("test", "http://node:8545")
	p := NewPool("test", []*BackendEndpoint{ep}, &FirstHealthySelector{}, "test")

	tripBreaker(t, p, ep)

	// Active health checker recovers the endpoint and logs it itself.
	ep.SetHealthy()

	// The pool must not report a second recovery on the next success.
	require.Nil(t, p.RecordResult(ep, 200, nil, 5))
}
