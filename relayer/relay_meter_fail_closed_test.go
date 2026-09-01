//go:build test

package relayer

import (
	"context"
	"testing"
	"time"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// newFailClosedMeter wires a meter whose only interesting property is which of
// its two dependencies is broken: its own store, or the chain behind it.
func newFailClosedMeter(t *testing.T, appAddrTheClientKnows string) (*RelayMeter, func()) {
	t.Helper()
	redisClient, _ := newTestRedis(t)

	app := &fakeAppClient{addr: appAddrTheClientKnows}
	app.stakeUpokt.Store(1000)

	meter := NewRelayMeter(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		redisClient,
		app,
		nil,
		&fakeSessionClient{numSuppliers: 1},
		nil,
		&fakeSharedParamCache{params: &sharedtypes.Params{
			NumBlocksPerSession:            10,
			ComputeUnitsToTokensMultiplier: 1,
			ComputeUnitCostGranularity:     1,
		}},
		nil,
		staticServiceFactor{f: 1},
		RelayMeterConfig{},
	)
	require.NoError(t, meter.Start(context.Background()))
	t.Cleanup(func() { _ = meter.Close() })

	// Breaking the store means closing the client: the test Redis is shared, so
	// stopping the server would take the rest of the suite with it.
	return meter, func() { _ = redisClient.Close() }
}

// TestTheStoreBeingUnreadableRefusesAdmission is one half of the rule that
// replaced relay_meter.fail_behavior on 2026-08-31. The meter's own store holds
// what this session has already consumed; without it the budget is unknown, and
// a relay whose budget is unknown is not admitted. There is no setting for this
// any more: the one that existed let a deployment choose to serve it.
func TestTheStoreBeingUnreadableRefusesAdmission(t *testing.T) {
	const appAddr = "pokt1app"
	meter, breakStore := newFailClosedMeter(t, appAddr)

	// Prove the happy path first, or the test cannot tell "refused" from
	// "never worked".
	allowed, err := meter.CheckAndConsumeRelay(
		context.Background(), "sess-1", appAddr, "svc", "pokt1supplier", 100, 91, 95)
	require.NoError(t, err)
	require.True(t, allowed, "precondition: a healthy meter admits the relay")

	breakStore()

	allowed, err = meter.CheckAndConsumeRelay(
		context.Background(), "sess-2", appAddr, "svc", "pokt1supplier", 100, 91, 95)

	require.Error(t, err)
	require.False(t, allowed,
		"the consumed counter is unreadable, so admission cannot know the budget")
	require.ErrorIs(t, err, ErrMeterStoreUnavailable,
		"the cause is marked at the call that failed: the callers reject on THIS "+
			"and serve on anything else, so an unmarked store failure would be served")
}

// TestOnlyTheChainBeingUnreachableStillAdmits is the other half, and it is the
// half that keeps a full-node blip from becoming a fleet-wide outage. The meter
// also reaches the chain -- getAppStake is a query -- and the miner re-derives
// what it needs when it claims, and retries. So the relay is served and passed
// on rather than refused.
func TestOnlyTheChainBeingUnreachableStillAdmits(t *testing.T) {
	// The app client knows a DIFFERENT address, so the stake query fails while
	// the store stays perfectly readable.
	meter, _ := newFailClosedMeter(t, "pokt1someone_else")

	allowed, err := meter.CheckAndConsumeRelay(
		context.Background(), "sess-1", "pokt1app", "svc", "pokt1supplier", 100, 91, 95)

	require.Error(t, err, "the failure is still reported -- it is not swallowed")
	require.True(t, allowed,
		"a chain query blinked, not our store: refusing here would turn an unstable "+
			"full node into a total outage, and the miner arbitrates anyway")
	require.NotErrorIs(t, err, ErrMeterStoreUnavailable,
		"nothing may mark a chain failure as a store failure, or it becomes a refusal")
}

// TestACorruptMeterMetaRefusesInsteadOfRecursing covers the third way the store
// can fail, which is not "unreachable" but "unreadable": the meta blob is there
// and does not parse.
//
// Two things used to go wrong at once. The unmarshal error was unmarked, so the
// policy counted it as the chain's and SERVED the relay with the consumed
// budget unknown. And getOrCreateSessionMeter discarded the read error and fell
// through to its create path, where SetNX reports the key already exists and the
// function calls itself again -- unbounded, on every relay of that session.
//
// MEASURED, restoring the discard: this test stops finishing and fails on the
// package timeout after ~60s. A stack overflow is the eventual end state but was
// NOT observed -- each level costs a store round trip, so the visible symptom is
// a relay that never answers, not a crash.
func TestACorruptMeterMetaRefusesInsteadOfRecursing(t *testing.T) {
	const appAddr = "pokt1app"
	const sessionID = "sess-corrupt"
	const supplier = "pokt1supplier"
	meter, _ := newFailClosedMeter(t, appAddr)
	ctx := context.Background()

	// Prove the happy path first, or the test cannot tell "refused" from
	// "never worked".
	allowed, err := meter.CheckAndConsumeRelay(ctx, "sess-ok", appAddr, "svc", supplier, 100, 91, 95)
	require.NoError(t, err)
	require.True(t, allowed, "precondition: a healthy meter admits the relay")

	// A meta blob that exists and does not parse.
	require.NoError(t, meter.redisClient.Set(
		ctx, meter.metaKey(sessionID, supplier), []byte("{not json"), time.Minute).Err())

	allowed, err = meter.CheckAndConsumeRelay(ctx, sessionID, appAddr, "svc", supplier, 100, 91, 95)

	require.Error(t, err)
	require.False(t, allowed,
		"the consumed counter cannot be trusted when its meta does not parse")
	require.ErrorIs(t, err, ErrMeterStoreUnavailable,
		"corruption in OUR store is the store's failure, not the chain's -- unmarked it is served")
}
