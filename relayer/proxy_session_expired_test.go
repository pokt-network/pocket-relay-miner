//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

// TestHandleRelay_SessionExpiredDispatchesReason proves the HTTP classifier
// dispatches an expired-session validation failure to relays_rejected_total
// under "session_expired", through the REAL entry point (handleRelay), not
// just at the validator/sentinel level. Between the validator and this
// classifier the error passes through validateRelayRequest's %w wrapping --
// a wrapper that drops the cause (like handleMeterError does for a different
// error) would make errors.Is here return false and this dispatch would never
// fire without anything going red, which is exactly what this test guards.
func TestHandleRelay_SessionExpiredDispatchesReason(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	f := newSimHTTPFixture(t, backend.URL, ValidationModeEager)
	f.proxy.validator = sessionExpiredValidator{}
	f.proxy.relayMeter = newAlwaysAllowMeter(t, f.appAddr)
	f.proxy.SetPublishQueueFull(func() bool { return false })

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-session-expired")

	expired := relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonSessionExpired)
	genericBefore := testutil.ToFloat64(relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonValidationFailed))
	expiredBefore := testutil.ToFloat64(expired)

	w := f.post(t, body, false) // setSimHeader=false: a REAL (non-simulated) relay

	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	require.Equal(t, expiredBefore+1, testutil.ToFloat64(expired),
		"session_expired must move by exactly one")
	require.Equal(t, genericBefore, testutil.ToFloat64(relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonValidationFailed)),
		"the generic validation_failed reason must NOT move for this rejection")
}

// newAlwaysAllowMeter builds a *RelayMeter over a real (test) Redis instance
// that always allows appAddr -- enough stake it never runs out inside a test.
// Reused across HTTP session_expired tests so the meter never becomes the
// reason a relay is rejected.
func newAlwaysAllowMeter(t *testing.T, appAddr string) *RelayMeter {
	t.Helper()
	redisClient, _ := newTestRedis(t)

	app := &fakeAppClient{addr: appAddr}
	app.stakeUpokt.Store(1_000_000)
	meter := NewRelayMeter(
		testLogger(), redisClient, app, nil,
		&fakeSessionClient{numSuppliers: 1}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}}, nil, staticServiceFactor{f: 1}, RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(context.Background()))
	t.Cleanup(func() { _ = meter.Close() })
	// Admission stays closed until a dispatcher heartbeat is wired, which would
	// make the meter the reason a relay is rejected; the charge writer wires one.
	newChargeWriter(t, meter, redisClient)
	return meter
}
