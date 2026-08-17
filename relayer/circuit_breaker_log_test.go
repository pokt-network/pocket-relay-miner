//go:build test

package relayer

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/rs/zerolog"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/pool"
)

// captureLogs returns a logger writing JSON lines into buf, so a test can
// assert on the fields an operator actually sees. Built directly on zerolog
// (logging.Logger is an alias) because the production constructor writes to
// stderr through an async diode, which a test cannot read back deterministically.
func captureLogs(t *testing.T) (logging.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	return zerolog.New(&buf), &buf
}

func lastLogLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	require.NotEmpty(t, lines, "expected a log line")
	var m map[string]any
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &m), "log line must be JSON: %s", lines[len(lines)-1])
	return m
}

// TestLogCircuitBreakerTransition_ReportsConfiguredThreshold proves the DOWN log
// carries the threshold the breaker actually evaluated, not the package default.
//
// Both copies of this function used to hardcode pool.DefaultUnhealthyThreshold
// in the log while the breaker was driven by the per-backend configured value.
// An operator who raised health_check.unhealthy_threshold to 20 read
// "consecutive_failures=20 threshold=5" and could not tell which number had
// tripped the breaker — during a failover, which is the only time anyone reads
// this line.
func TestLogCircuitBreakerTransition_ReportsConfiguredThreshold(t *testing.T) {
	const configuredThreshold int32 = 20
	require.NotEqual(t, pool.DefaultUnhealthyThreshold, configuredThreshold,
		"the test is meaningless unless it differs from the default")

	logger, buf := captureLogs(t)

	transition := &pool.TransitionEvent{
		Endpoint:   &pool.BackendEndpoint{Name: "backend-a", RawURL: "http://backend-a:8545"},
		OldHealthy: true,
		NewHealthy: false,
		Failures:   configuredThreshold,
		StatusCode: 503,
		Error:      errors.New("upstream refused"),
	}

	logCircuitBreakerTransition(logger, transition, "svc-cb", "3", configuredThreshold)

	fields := lastLogLine(t, buf)
	require.EqualValues(t, configuredThreshold, fields["threshold"],
		"the log must report the threshold the breaker used, not the default")
	require.EqualValues(t, configuredThreshold, fields["consecutive_failures"])
	require.Equal(t, "svc-cb", fields["service_id"])
	require.Equal(t, "backend-a", fields["backend"])
	require.EqualValues(t, 503, fields["trigger_http_status"])
	require.Equal(t, "upstream refused", fields["trigger_error"])
	require.Contains(t, fields["message"], "BACKEND DOWN")
}

// TestLogCircuitBreakerTransition_Recovery proves the UP line reports downtime
// and does not carry the DOWN-only fields.
func TestLogCircuitBreakerTransition_Recovery(t *testing.T) {
	logger, buf := captureLogs(t)

	transition := &pool.TransitionEvent{
		Endpoint:         &pool.BackendEndpoint{Name: "backend-b", RawURL: "http://backend-b:8545"},
		OldHealthy:       false,
		NewHealthy:       true,
		DowntimeDuration: 90 * time.Second,
		StatusCode:       200,
	}

	logCircuitBreakerTransition(logger, transition, "svc-cb", "websocket", 20)

	fields := lastLogLine(t, buf)
	require.Contains(t, fields["message"], "BACKEND UP")
	require.Equal(t, "websocket", fields["rpc_type"])
	require.EqualValues(t, 200, fields["recovery_http_status"])
	require.NotContains(t, fields, "consecutive_failures",
		"a recovery is not a failure count")
}

// TestLogCircuitBreakerTransition_NoTransitionIsSilent proves an event that is
// not a state change logs nothing. The breaker calls this on every recorded
// result, so emitting here would put a line on the hot path.
func TestLogCircuitBreakerTransition_NoTransitionIsSilent(t *testing.T) {
	logger, buf := captureLogs(t)

	for _, tc := range []struct{ old, new bool }{
		{old: true, new: true},
		{old: false, new: false},
	} {
		logCircuitBreakerTransition(logger, &pool.TransitionEvent{
			Endpoint:   &pool.BackendEndpoint{Name: "backend-c"},
			OldHealthy: tc.old,
			NewHealthy: tc.new,
		}, "svc-cb", "3", 20)
	}

	require.Empty(t, bytes.TrimSpace(buf.Bytes()),
		"a non-transition must not log")
}
