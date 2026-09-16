//go:build test

package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/alitto/pond/v2"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// gatedFixture is newSimHTTPFixture with a live meter, so a relay the gates let
// through reaches the backend (optimistic) or the eager meter.
func gatedFixture(t *testing.T, mode ValidationMode) (*simHTTPFixture, *atomic.Int32) {
	t.Helper()
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	t.Cleanup(backend.Close)
	f := newSimHTTPFixture(t, backend.URL, mode)
	meterRedis, _ := newTestRedis(t)
	app := &fakeAppClient{addr: f.appAddr}
	app.stakeUpokt.Store(1_000_000)
	meter := NewRelayMeter(logging.NewLoggerFromConfig(logging.DefaultConfig()), meterRedis, app, nil,
		&fakeSessionClient{numSuppliers: 1}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{NumBlocksPerSession: 10, ComputeUnitsToTokensMultiplier: 1, ComputeUnitCostGranularity: 1}},
		nil, staticServiceFactor{f: 1}, RelayMeterConfig{})
	require.NoError(t, meter.Start(context.Background()))
	t.Cleanup(func() { _ = meter.Close() })
	newChargeWriter(t, meter, meterRedis)
	f.proxy.SetRelayMeter(meter)
	f.proxy.validator = &neverCallValidator{}
	// The fixture wires no validation pool; the optimistic path submits to one.
	pool := pond.NewPool(1)
	t.Cleanup(pool.StopAndWait)
	f.proxy.validationSubpool = pool
	return f, &backendHits
}

func validationQueueRejections() float64 {
	return testutil.ToFloat64(relaysRejected.WithLabelValues(simTestService, BackendTypeJSONRPC, rejectReasonValidationQueueFull))
}

func TestOptimisticRelayIsRefusedBeforeTheBackendWhileTheValidationQueueIsFull(t *testing.T) {
	f, backendHits := gatedFixture(t, ValidationModeOptimistic)
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-validation-queue-full")
	before := validationQueueRejections()

	f.proxy.validationQueuedBytes.Store(maxValidationQueuedBytes)
	w := f.post(t, body, false)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "LINK validation-queue-429: a full validation queue answers 429; body=%s", w.Body.String())
	require.Equal(t, "1", w.Header().Get("Retry-After"), "LINK validation-queue-429: with Retry-After")
	require.Equal(t, before+1, validationQueueRejections())
	require.Zero(t, backendHits.Load(), "LINK validation-queue: a refused optimistic relay is never served")

	f.proxy.validationQueuedBytes.Store(maxValidationQueuedBytes - 1)
	w = f.post(t, body, false)
	require.Equal(t, int32(1), backendHits.Load(), "control: below the limit the same relay is served; code=%d body=%s", w.Code, w.Body.String())
	require.Equal(t, before+1, validationQueueRejections())
}

func TestEagerRelayIsNotRefusedByTheValidationQueue(t *testing.T) {
	f, _ := gatedFixture(t, ValidationModeEager)
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-validation-queue-eager")
	before := validationQueueRejections()

	f.proxy.validationQueuedBytes.Store(maxValidationQueuedBytes)
	w := f.post(t, body, false)
	require.Equal(t, http.StatusForbidden, w.Code,
		"LINK validation-queue-eager: an eager relay passes the queue gate and reaches validation; body=%s", w.Body.String())
	require.Equal(t, before, validationQueueRejections())
}

func TestOptimisticValidationReleasesItsQueuedBytes(t *testing.T) {
	f, backendHits := gatedFixture(t, ValidationModeOptimistic)
	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-validation-queue-release")
	w := f.post(t, body, false)
	require.Equal(t, int32(1), backendHits.Load(), "code=%d body=%s", w.Code, w.Body.String())
	f.proxy.validationSubpool.StopAndWait()
	require.Zero(t, f.proxy.validationQueuedBytes.Load(), "LINK validation-release: a validated relay gives its bytes back")
}
