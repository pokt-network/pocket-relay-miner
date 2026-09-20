//go:build test

package relayer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alitto/pond/v2"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Admission and charging, end to end: a relay reserves its cost when admitted,
// the reservation becomes a charge when the relay is served and is given back
// when it is not, and the batch dispatcher writes the charges to Redis.

const (
	chargeTestApp      = "pokt1app_charge"
	chargeTestService  = "svc-charge"
	chargeTestSupplier = "pokt1supplier_charge"

	// chargeTestCap is the per-supplier budget of newChargeTestMeter at 1 uPOKT a
	// relay: app stake 1000, service factor 0.5, two suppliers -- the parameters
	// TestCheckAndConsumeRelay_PerSupplierIsolation pins at 500.
	chargeTestCap = int64(500)
)

// newChargeTestMeter builds a meter on the test Redis. start is false for a test
// that counts Redis commands: a started meter runs background loops of its own.
func newChargeTestMeter(t *testing.T, start bool) (*RelayMeter, *redisutil.Client) {
	t.Helper()
	redisClient, _ := newTestRedis(t)
	return newChargeMeterOn(t, redisClient, start), redisClient
}

// newChargeMeterOn is newChargeTestMeter on a given client, for tests where two
// meters share one namespace.
func newChargeMeterOn(t *testing.T, redisClient *redisutil.Client, start bool) *RelayMeter {
	t.Helper()
	app := &fakeAppClient{addr: chargeTestApp}
	app.stakeUpokt.Store(1000)
	meter := NewRelayMeter(
		testLogger(), redisClient, app, nil,
		&fakeSessionClient{numSuppliers: 2}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}},
		nil, staticServiceFactor{f: 0.5}, RelayMeterConfig{},
	)
	if start {
		require.NoError(t, meter.Start(context.Background()))
	}
	t.Cleanup(func() { _ = meter.Close() })
	return meter
}

func admitCharge(t *testing.T, meter *RelayMeter, sessionID string) (Reservation, bool) {
	t.Helper()
	reservation, allowed, err := meter.Admit(context.Background(), sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	return reservation, allowed
}

func consumedIn(t *testing.T, client *redisutil.Client, key string) int64 {
	t.Helper()
	consumed, err := client.Get(context.Background(), key).Int64()
	if errors.Is(err, goredis.Nil) {
		return 0
	}
	require.NoError(t, err)
	return consumed
}

func inFlightOf(meter *RelayMeter, key string) int64 {
	meter.accMu.Lock()
	defer meter.accMu.Unlock()
	return meter.inFlight[key]
}

// TestAdmissionCountsRelaysStillBeingServed holds every reservation until all the
// admissions are decided, the way a slow backend does. Nothing is written to
// Redis in that window, so only what admitted relays hold can stop the pair
// going over its budget.
func TestAdmissionCountsRelaysStillBeingServed(t *testing.T) {
	meter, rc := newChargeTestMeter(t, true)
	charges := newChargeWriter(t, meter, rc)
	const sessionID = "sess-held"
	key := meter.consumedKey(sessionID, chargeTestSupplier)

	attempts := int(3 * chargeTestCap)
	admitted := make(chan Reservation, attempts)
	var refused, failed atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reservation, allowed, err := meter.Admit(context.Background(), sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
			switch {
			case err != nil:
				failed.Add(1)
			case allowed:
				admitted <- reservation
			default:
				refused.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(admitted)

	require.Zero(t, failed.Load())
	require.Equal(t, int(chargeTestCap), len(admitted),
		"every admitted relay holds its cost until it is served, so exactly the budget is admitted")
	require.Equal(t, 2*chargeTestCap, refused.Load())

	for reservation := range admitted {
		meter.Settle(reservation)
	}
	charges.flush()

	require.Equal(t, chargeTestCap, consumedIn(t, rc, key), "every served relay is charged once")
	require.Zero(t, inFlightOf(meter, key))
}

// newEagerChargeFixture is the HTTP fixture with a real meter, its charge writer
// and the given validator, in eager mode.
func newEagerChargeFixture(t *testing.T, backendURL string, validator RelayValidator) (*simHTTPFixture, *RelayMeter, *chargeWriter, *redisutil.Client) {
	t.Helper()
	f := newSimHTTPFixture(t, backendURL, ValidationModeEager)

	meterRedis, _ := newTestRedis(t)
	app := &fakeAppClient{addr: f.appAddr}
	app.stakeUpokt.Store(1_000_000)
	meter := NewRelayMeter(
		testLogger(), meterRedis, app, nil,
		&fakeSessionClient{numSuppliers: 1}, nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}},
		nil, staticServiceFactor{f: 1}, RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(context.Background()))
	t.Cleanup(func() { _ = meter.Close() })
	charges := newChargeWriter(t, meter, meterRedis)
	f.proxy.SetRelayMeter(meter)
	f.proxy.validator = validator
	return f, meter, charges, meterRedis
}

// TestAnEagerRelayRefusedForItsSignatureIsNotCharged: eager admits before it
// validates, so a forged relay reaches the meter. Charging it would let anyone
// who can name an application spend that application's budget.
func TestAnEagerRelayRefusedForItsSignatureIsNotCharged(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	validator := &neverCallValidator{}
	f, meter, charges, rc := newEagerChargeFixture(t, backend.URL, validator)
	const sessionID = "sess-forged"
	key := meter.consumedKey(sessionID, f.supplierAddr)
	admitted := eagerAdmissions(simTestService)

	w := f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService, sessionID), false)

	require.Equal(t, http.StatusForbidden, w.Code, "body=%s", w.Body.String())
	require.Equal(t, int32(1), validator.calls.Load())
	require.Equal(t, admitted+1, eagerAdmissions(simTestService), "premise: the relay was admitted before validation refused it")
	require.Zero(t, inFlightOf(meter, key), "the refused relay must give its reservation back")

	charges.flush()
	require.False(t, keyExists(t, rc, key), "a relay refused for its signature must not consume the budget")
	require.Equal(t, int32(0), backendHits.Load())
}

// TestEagerRelaysTheBackendFailedAreNotCharged: a relay whose backend failed is
// not served, not mined and not paid by the chain, so it must not consume the
// budget the supplier can claim.
func TestEagerRelaysTheBackendFailedAreNotCharged(t *testing.T) {
	var backendHits atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"down"},"id":1}`))
	}))
	defer backend.Close()

	f, meter, charges, rc := newEagerChargeFixture(t, backend.URL, acceptAnyValidator{})
	const sessionID = "sess-backend-down"
	key := meter.consumedKey(sessionID, f.supplierAddr)
	admitted := eagerAdmissions(simTestService)

	const failures = 3
	for i := 0; i < failures; i++ {
		f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService, sessionID), false)
	}

	require.Equal(t, int32(failures), backendHits.Load(), "premise: every relay reached the backend")
	require.Equal(t, admitted+failures, eagerAdmissions(simTestService))
	require.Zero(t, inFlightOf(meter, key), "every relay the backend failed must give its reservation back")

	charges.flush()
	require.False(t, keyExists(t, rc, key), "relays that were not served must not consume the budget")
}

// TestEveryServedRelayIsChargedWhetherOrNotItWasMined: the chain pays leaves
// times the difficulty multiplier, so a relay that misses the target is still
// paid for and must still be charged. With a zero target nothing is mined.
func TestEveryServedRelayIsChargedWhetherOrNotItWasMined(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	defer backend.Close()

	f, meter, charges, rc := newEagerChargeFixture(t, backend.URL, acceptAnyValidator{})
	processor := NewRelayProcessor(testLogger(), nil, nil, nil)
	processor.SetDifficultyProvider(zeroTargetDifficulty{})
	f.proxy.relayProcessor = processor
	workers := pond.NewPool(2)
	t.Cleanup(workers.StopAndWait)
	f.proxy.publishSubpool = workers.NewSubpool(1)

	const sessionID = "sess-unmined"
	key := meter.consumedKey(sessionID, f.supplierAddr)
	skipped := relaysSkippedDifficulty.WithLabelValues(simTestService, BackendTypeJSONRPC)
	skippedBefore := testutil.ToFloat64(skipped)

	const served = 4
	for i := 0; i < served; i++ {
		w := f.post(t, f.buildSignedSimBody(t, f.appAddr, simTestService, sessionID), false)
		require.Equal(t, http.StatusOK, w.Code, "body=%s", w.Body.String())
	}
	// Every publish task has run once the subpool drains: the count below is final.
	f.proxy.publishSubpool.StopAndWait()

	require.Equal(t, skippedBefore+served, testutil.ToFloat64(skipped), "premise: no served relay met the difficulty")
	require.Equal(t, int32(0), f.pub.calls.Load(), "premise: nothing was mined, so nothing was published")

	charges.flush()
	require.Equal(t, int64(served), consumedIn(t, rc, key), "every served relay is charged, mined or not")
}

// TestTheFirstAdmissionOfAPairRespectsWhatRedisHolds: a replica that has never
// seen the pair -- a new replica, or this one after a restart -- must start from
// what was already charged, not from zero.
func TestTheFirstAdmissionOfAPairRespectsWhatRedisHolds(t *testing.T) {
	meter, rc := newChargeTestMeter(t, true)
	newChargeWriter(t, meter, rc)
	const sessionID = "sess-restarted"
	key := meter.consumedKey(sessionID, chargeTestSupplier)
	require.NoError(t, rc.Set(context.Background(), key, chargeTestCap-2, time.Hour).Err())

	for i := 0; i < 2; i++ {
		_, allowed := admitCharge(t, meter, sessionID)
		require.True(t, allowed, "relay %d fits in what Redis says is left", i+1)
	}
	_, allowed := admitCharge(t, meter, sessionID)
	require.False(t, allowed, "the budget Redis holds is spent; admitting more would charge the app twice")
	require.Equal(t, chargeTestCap-2, consumedIn(t, rc, key), "admission reads the counter and never rewrites it")
}

// dispatcherReachingRedis is the answer of a dispatcher that still reaches
// Redis: what is served now will be charged.
func dispatcherReachingRedis() (bool, error) { return true, nil }

// TestAdmissionClosesWhenTheDispatcherStopsReachingRedis: the dispatcher writes
// what is served, so admission follows its heartbeat, not the relay traffic.
func TestAdmissionClosesWhenTheDispatcherStopsReachingRedis(t *testing.T) {
	meter, _ := newChargeTestMeter(t, true)
	const sessionID = "sess-heartbeat"
	key := meter.consumedKey(sessionID, chargeTestSupplier)
	ctx := context.Background()

	_, allowed, err := meter.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.ErrorIs(t, err, ErrMeterStoreUnavailable, "with no dispatcher wired nothing would write the charge")
	require.False(t, allowed)

	// The meter compares no instants of its own: it asks the dispatcher and
	// obeys. How long a silence is tolerated is the dispatcher's budget, pinned
	// in transport/redis where the instants live.
	healthy, reason := true, error(nil)
	meter.SetDispatcherHealth(func() (bool, error) { return healthy, reason })

	reservation, allowed, err := meter.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed, "a dispatcher still reaching Redis keeps admission open")
	meter.Release(reservation)

	healthy, reason = false, errors.New("last answer 12s ago, budget 10s")
	_, allowed, err = meter.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.ErrorIs(t, err, ErrMeterStoreUnavailable, "with the dispatcher silent a relay admitted now may never be charged")
	require.NotContains(t, err.Error(), "last answer 12s ago",
		"the dispatcher's reason stays in the Debug log and the metric: what crosses this boundary reaches a "+
			"client's 503 body, and our budget and store are not a client's business")
	require.False(t, allowed)
	require.Zero(t, inFlightOf(meter, key), "a refused relay reserves nothing")

	// The dispatcher reaches Redis again: nothing in the meter has to age out.
	healthy, reason = true, nil
	_, allowed, err = meter.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed)
}

// TestAChargeAfterTheSessionIsClearedExpiresAndDoesNotReviveThePair: a relay
// served after its session was cleared is still charged, and the counter that
// charge creates must expire. The view must not take the pair back either.
func TestAChargeAfterTheSessionIsClearedExpiresAndDoesNotReviveThePair(t *testing.T) {
	meter, rc := newChargeTestMeter(t, true)
	charges := newChargeWriter(t, meter, rc)
	const sessionID = "sess-cleared"
	key := meter.consumedKey(sessionID, chargeTestSupplier)
	ctx := context.Background()

	reservation, allowed := admitCharge(t, meter, sessionID)
	require.True(t, allowed)
	require.NoError(t, meter.ClearSessionMeter(ctx, sessionID, chargeTestSupplier))

	meter.Settle(reservation)
	charges.flush()

	require.Equal(t, int64(1), consumedIn(t, rc, key), "the served relay is charged")
	ttl, err := rc.TTL(ctx, key).Result()
	require.NoError(t, err)
	require.Positive(t, ttl, "the counter a late charge recreates must expire (got %s)", ttl)

	meter.accMu.Lock()
	_, viewed := meter.seen[key]
	meter.accMu.Unlock()
	require.False(t, viewed, "a charge written after the cleanup must not bring the pair back into the view")
}

// TestAnOptimisticRelayOverTheBudgetAddsNothingToTheLedger: optimistic charges
// after serving, and over the budget it drops the relay without charging it,
// as before.
func TestAnOptimisticRelayOverTheBudgetAddsNothingToTheLedger(t *testing.T) {
	meter, rc := newChargeTestMeter(t, true)
	newChargeWriter(t, meter, rc)
	const sessionID = "sess-optimistic-over"
	key := meter.consumedKey(sessionID, chargeTestSupplier)
	require.NoError(t, rc.Set(context.Background(), key, chargeTestCap, time.Hour).Err())

	allowed, err := meter.CheckAndConsumeRelay(context.Background(), sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)

	require.NoError(t, err)
	require.False(t, allowed)
	require.Zero(t, meter.ChargeLedger().Pending(key), "a relay over the budget adds nothing to what will be written")
	require.Zero(t, inFlightOf(meter, key))
}

// TestAWebSocketChargesEachBackendMessageAndNotTheFrame: a client frame is only
// checked against the budget. Each message the backend answers it with is a
// relay and is charged, and a frame the backend never answers costs nothing.
func TestAWebSocketChargesEachBackendMessageAndNotTheFrame(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	const pushes = 4
	pipeline, rc, _, charges := newOwnerTestPipelineWithCharges(t)
	meter := pipeline.relayMeter
	supplier, signer := newSupplier(t)

	answered := newPublishSignal()
	conn := newPublishingSageBridge(t, pushingWSBackend(t, pushes), signer, pipeline, answered)
	sendRelay(t, conn, ownerTestRelay("ws-answered", supplier))
	for i := 0; i < pushes; i++ {
		readServedResponse(t, conn)
	}
	answered.await(t, pushes)

	checked := relayMeterConsumptions.WithLabelValues(simWSTestService, "within_limit")
	checksBefore := testutil.ToFloat64(checked)
	silentURL, received := silentWSBackend(t)
	silent := newSageShapedBridge(t, silentURL, signer, pipeline)
	sendRelay(t, silent, ownerTestRelay("ws-unanswered", supplier))
	awaitSignal(t, received, "the unanswered frame reaching the backend")
	charges.flush()

	require.Equal(t, int64(pushes), consumedIn(t, rc, rc.KB().MeterConsumedKey("ws-answered", supplier)),
		"every message the backend answered with is charged, and the frame is not")
	require.Equal(t, checksBefore+1, testutil.ToFloat64(checked),
		"premise: the unanswered frame went through the budget check")
	require.Zero(t, consumedIn(t, rc, rc.KB().MeterConsumedKey("ws-unanswered", supplier)),
		"a frame the backend never answered costs nothing")
	require.Zero(t, inFlightOf(meter, meter.consumedKey("ws-unanswered", supplier)),
		"and holds nothing against the budget")
}

// TestAChargeWithoutAdmissionDoesNotBringAClearedPairBack: a WebSocket backend
// message is charged without reading the pair's counter. After the session is
// cleared it is charged and reported under budget, whatever Redis holds, and the
// pair stays out of the view.
func TestAChargeWithoutAdmissionDoesNotBringAClearedPairBack(t *testing.T) {
	meter, rc := newChargeTestMeter(t, true)
	newChargeWriter(t, meter, rc)
	const sessionID = "sess-push-after-clear"
	ctx := context.Background()
	key := meter.consumedKey(sessionID, chargeTestSupplier)

	allowed, err := meter.CheckBudget(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed, "premise: the frame brought the pair into the view")
	require.NoError(t, meter.ClearSessionMeter(ctx, sessionID, chargeTestSupplier))
	require.NoError(t, rc.Set(ctx, key, chargeTestCap, time.Hour).Err())

	atBudget, err := meter.ChargeServed(ctx, sessionID, chargeTestService, chargeTestSupplier, 91)

	require.NoError(t, err)
	require.False(t, atBudget, "a pair outside the view is charged without checking its budget")
	require.Equal(t, int64(1), meter.ChargeLedger().Pending(key), "the served message is charged")
	meter.accMu.Lock()
	_, viewed := meter.seen[key]
	meter.accMu.Unlock()
	require.False(t, viewed, "charging must not read the counter back and revive the cleared pair")
}

// commandCounter counts every command the client sends, inside a pipeline or not.
type commandCounter struct{ commands atomic.Int64 }

func (c *commandCounter) DialHook(next goredis.DialHook) goredis.DialHook { return next }
func (c *commandCounter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		c.commands.Add(1)
		return next(ctx, cmd)
	}
}

func (c *commandCounter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		c.commands.Add(int64(len(cmds)))
		return next(ctx, cmds)
	}
}

// TestAnOlderWriteReplyDoesNotLowerTheView: two writes of one pair are answered
// out of order, the newer counter value first. The view keeps the higher one; a
// view lowered to the older reply would admit what Redis already counts.
func TestAnOlderWriteReplyDoesNotLowerTheView(t *testing.T) {
	meter, _ := newChargeTestMeter(t, false)
	meter.SetDispatcherHealth(dispatcherReachingRedis)
	const sessionID = "sess-replies"
	reservation, allowed, err := meter.Admit(context.Background(), sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed, "premise: the pair is in the view")
	meter.Release(reservation)
	key := meter.consumedKey(sessionID, chargeTestSupplier)

	meter.chargeWritten(key, 50, 150)
	meter.chargeWritten(key, 50, 100)

	meter.accMu.Lock()
	seen := meter.seen[key]
	meter.accMu.Unlock()
	require.Equal(t, int64(150), seen, "the reply of the older write must not lower the view")
}

// TestAViewedPairIsAdmittedAndChargedWithoutTouchingRedis: once a replica has
// seen a pair, admitting and charging a relay is memory only. The writes are the
// dispatcher's, one round trip per tick, not one per relay.
func TestAViewedPairIsAdmittedAndChargedWithoutTouchingRedis(t *testing.T) {
	meter, rc := newChargeTestMeter(t, false)
	// No dispatcher here, so the count below is the meter's alone.
	meter.SetDispatcherHealth(dispatcherReachingRedis)
	const sessionID = "sess-hot"
	ctx := context.Background()

	allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed, "premise: the first relay reads the pair from Redis")

	counter := &commandCounter{}
	rc.AddHook(counter)

	const relays = 50
	for i := 0; i < relays; i++ {
		allowed, err := meter.CheckAndConsumeRelay(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
		require.NoError(t, err)
		require.True(t, allowed)
	}

	require.Zero(t, counter.commands.Load(), "a viewed pair must not cost a Redis round trip per relay")
	require.Equal(t, int64(relays+1), meter.ChargeLedger().Pending(meter.consumedKey(sessionID, chargeTestSupplier)),
		"control: every relay was charged, into the ledger")
}

// TestGRPCChargesTheServedRelayAndGivesBackTheFailedOne: gRPC admits before it
// serves, like eager HTTP. The relay that reaches the client is charged; the one
// whose backend failed gives its reservation back.
func TestGRPCChargesTheServedRelayAndGivesBackTheFailedOne(t *testing.T) {
	var failing atomic.Bool
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	defer backend.Close()

	fx := newGRPCPublishFixture(t, backend.URL)
	meter := fx.pipeline.relayMeter
	key := meter.consumedKey(fx.stream.req.Meta.SessionHeader.SessionId, fx.supplier)

	require.NoError(t, fx.svc.handleSendRelay(fx.stream), "premise: the first relay is served")
	failing.Store(true)
	require.Error(t, fx.svc.handleSendRelay(&mockServerStream{ctx: fx.stream.ctx, req: fx.stream.req}),
		"premise: the second relay's backend failed")

	require.Zero(t, inFlightOf(meter, key), "the relay that was not served must give its reservation back")
	fx.charges.flush()
	require.Equal(t, int64(1), consumedIn(t, fx.redis, key), "only the served relay is charged")
}
