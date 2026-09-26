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

	fillQueue(t, f, simTestService)
	w := f.post(t, body, false)
	require.Equal(t, http.StatusTooManyRequests, w.Code, "LINK validation-queue-429: a full validation queue answers 429; body=%s", w.Body.String())
	require.Equal(t, "1", w.Header().Get("Retry-After"), "LINK validation-queue-429: with Retry-After")
	require.Equal(t, before+1, validationQueueRejections())
	require.Zero(t, backendHits.Load(), "LINK validation-queue: a refused optimistic relay is never served")

	f.proxy.validationQueueFor(simTestService).queued.Store(0)
	w = f.post(t, body, false)
	require.Equal(t, int32(1), backendHits.Load(), "control: below the limit the same relay is served; code=%d body=%s", w.Code, w.Body.String())
	require.Equal(t, before+1, validationQueueRejections())
}

// TestEagerRelayIsNotRefusedByTheValidationQueue pins that the gate cannot
// refuse a service that does not queue -- and it asserts WHY, because under
// this design the reason IS the construction.
//
// An eager service gets no queue at all, so there is nothing to fill and
// nothing that can report itself full. "Eager relays pass the gate" is
// therefore not a branch anyone has to maintain: it is what having no queue
// means. This test used to fill a queue first, which asserted the branch and
// assumed the queue existed -- an assumption that stopped being true the day
// the bound became per service.
func TestEagerRelayIsNotRefusedByTheValidationQueue(t *testing.T) {
	f, _ := gatedFixture(t, ValidationModeEager)
	require.Nil(t, f.proxy.validationQueueFor(simTestService),
		"LINK cap-eager-has-no-queue: an eager service is given no validation queue, and that "+
			"-- not a check on the mode at admission -- is what makes its relays unrefusable here")

	body := f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-validation-queue-eager")
	before := validationQueueRejections()

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
	require.Zero(t, f.proxy.validationQueueFor(simTestService).queued.Load(),
		"LINK validation-release: a validated relay gives its bytes back")
}

// fillQueue puts one service at its own bound, which is the only way to refuse
// it: there is no global bound left to fill.
func fillQueue(t *testing.T, f *simHTTPFixture, serviceID string) {
	t.Helper()
	q := f.proxy.validationQueueFor(serviceID)
	require.NotNil(t, q, "service %q has no validation queue, so it can never be refused", serviceID)
	q.queued.Store(q.maxBytes)
}

// TestTheBoundIsPerServiceAndTheRejectionNamesTheServiceThatFilledIt is the
// case a global bound could not express, and it demonstrates the three things
// the bound was asked for at once: the DEFAULT applies to a service that says
// nothing, an OVERRIDE is respected for one that does, and a service at its own
// bound is refused WHILE ANOTHER IS STILL SERVED.
//
// Under one global bound the relay that ARRIVES pays for the bytes another
// service is HOLDING, so "who was refused" and "who filled it" were different
// questions. Here they are the same service by construction.
func TestTheBoundIsPerServiceAndTheRejectionNamesTheServiceThatFilledIt(t *testing.T) {
	f, backendHits := gatedFixture(t, ValidationModeOptimistic)

	// The resolved bounds: one from the default, one from its own override.
	full := f.proxy.validationQueueFor(simTestService)
	other := f.proxy.validationQueueFor(simTestService2)
	require.NotNil(t, full)
	require.NotNil(t, other)
	require.Equal(t, int64(DefaultValidationQueueMaxMiB)<<20, full.maxBytes,
		"LINK cap-default: a service that configures nothing takes the default")
	require.Equal(t, int64(simTestService2QueueMiB)<<20, other.maxBytes,
		"LINK cap-override: a service that configures its own bound gets THAT one, not the default")

	before := validationQueueRejections()
	fillQueue(t, f, simTestService)

	// The service that filled its bound is refused...
	refused := f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService, "sess-cap-own"), false)
	require.Equal(t, http.StatusTooManyRequests, refused.Code,
		"LINK cap-refuses: a service over its own bound is refused; body=%s", refused.Body.String())
	require.Equal(t, before+1, validationQueueRejections(),
		"LINK cap-attribution: the rejection is counted for the service that was over ITS bound")
	require.Zero(t, backendHits.Load(), "a refused relay never reaches the backend")

	// ...and the other one, holding nothing, is served in the same instant.
	served := f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService2, "sess-cap-other"), false)
	require.Equal(t, int32(1), backendHits.Load(),
		"LINK cap-isolation: another service is NOT refused by a queue it is not filling; code=%d body=%s",
		served.Code, served.Body.String())
	require.Equal(t, before+1, validationQueueRejections(),
		"LINK cap-isolation: and its relay is not counted as a queue rejection")
}
