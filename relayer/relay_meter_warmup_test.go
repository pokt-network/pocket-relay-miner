//go:build test

package relayer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// A relayer that restarts warms its meter from Redis: the pairs this replica
// already meters are read in rounds and put into the view, so their first relays
// admit from memory.

// newTestRedisOnPrefix opens another client on an existing test namespace, so
// one meter can write live meters and another read them back while each
// client's commands are counted apart.
func newTestRedisOnPrefix(t *testing.T, prefix string) *redisutil.Client {
	t.Helper()
	client, err := redisutil.NewClient(context.Background(), redisutil.ClientConfig{
		URL:       testredis.URL(),
		Namespace: config.RedisNamespaceConfig{BasePrefix: prefix},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// meterLivePairs admits and serves one relay for each session on supplier
// through the writer, and writes the charges, the way a running relayer leaves
// Redis.
func meterLivePairs(t *testing.T, writer *RelayMeter, charges *chargeWriter, supplier string, sessions []string) {
	t.Helper()
	for _, sessionID := range sessions {
		reservation, allowed, err := writer.Admit(context.Background(), sessionID, chargeTestApp, chargeTestService, supplier, 91, 100, 0)
		require.NoError(t, err)
		require.True(t, allowed)
		writer.Settle(reservation)
	}
	charges.flush()
}

func signsOnly(supplier string) func(string) bool {
	return func(s string) bool { return s == supplier }
}

func viewed(meter *RelayMeter, key string) (int64, bool) {
	meter.accMu.Lock()
	defer meter.accMu.Unlock()
	v, ok := meter.seen[key]
	return v, ok
}

func viewedMeta(meter *RelayMeter, sessionID, supplier string) *SessionMeterMeta {
	meter.localCacheMu.RLock()
	defer meter.localCacheMu.RUnlock()
	return meter.localCache[localCacheKey(sessionID, supplier)]
}

// TestAWarmedPairIsAdmittedWithoutTouchingRedis writes 300 live pairs, more than
// one round, and restarts: every warmed pair's first admission sends Redis
// nothing, neither for its meta nor for its consumed counter.
func TestAWarmedPairIsAdmittedWithoutTouchingRedis(t *testing.T) {
	ctx := context.Background()
	writerRedis, prefix := newTestRedis(t)
	writer := newChargeMeterOn(t, writerRedis, true)
	charges := newChargeWriter(t, writer, writerRedis)

	const pairs = 300
	sessions := make([]string, pairs)
	for i := range sessions {
		sessions[i] = fmt.Sprintf("sess-warm-%03d", i)
	}
	meterLivePairs(t, writer, charges, chargeTestSupplier, sessions)

	readerRedis := newTestRedisOnPrefix(t, prefix)
	reader := newChargeMeterOn(t, readerRedis, false)
	reader.SetDispatcherHeartbeat(time.Now)

	warmed, err := reader.WarmFromRedis(ctx, signsOnly(chargeTestSupplier))
	require.NoError(t, err)
	require.Equal(t, pairs, warmed)
	consumed, ok := viewed(reader, reader.consumedKey(sessions[pairs-1], chargeTestSupplier))
	require.True(t, ok, "the last pair of the last round is in the view")
	require.Equal(t, int64(1), consumed, "with the counter Redis holds")

	counter := &commandCounter{}
	readerRedis.AddHook(counter)
	for _, sessionID := range sessions {
		_, allowed, err := reader.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
		require.NoError(t, err)
		require.True(t, allowed)
	}
	require.Zero(t, counter.commands.Load(), "a warmed pair must be admitted without a Redis round trip")

	cold := newChargeMeterOn(t, readerRedis, false)
	cold.SetDispatcherHeartbeat(time.Now)
	_, allowed, err := cold.Admit(ctx, sessions[0], chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Positive(t, counter.commands.Load(), "control: a meter that was not warmed reads the pair from Redis")
}

// TestWarmupLeavesOutPairsThisReplicaCannotServe: a pair of a supplier this
// replica does not sign for, and a tracked pair whose meta is gone, stay out of
// the view.
func TestWarmupLeavesOutPairsThisReplicaCannotServe(t *testing.T) {
	ctx := context.Background()
	writerRedis, prefix := newTestRedis(t)
	writer := newChargeMeterOn(t, writerRedis, true)
	charges := newChargeWriter(t, writer, writerRedis)
	const otherSupplier = "pokt1supplier_other"
	meterLivePairs(t, writer, charges, chargeTestSupplier, []string{"sess-mine"})
	meterLivePairs(t, writer, charges, otherSupplier, []string{"sess-theirs"})
	require.NoError(t, writerRedis.SAdd(ctx, writerRedis.KB().MeterActiveSessionsKey(), localCacheKey("sess-gone", chargeTestSupplier)).Err())

	reader := newChargeMeterOn(t, newTestRedisOnPrefix(t, prefix), false)
	warmed, err := reader.WarmFromRedis(ctx, signsOnly(chargeTestSupplier))

	require.NoError(t, err)
	require.Equal(t, 1, warmed)
	_, ok := viewed(reader, reader.consumedKey("sess-mine", chargeTestSupplier))
	require.True(t, ok, "control: this replica's live pair is warmed")
	_, ok = viewed(reader, reader.consumedKey("sess-theirs", otherSupplier))
	require.False(t, ok, "a pair of a supplier this replica does not sign for stays out of the view")
	_, ok = viewed(reader, reader.consumedKey("sess-gone", chargeTestSupplier))
	require.False(t, ok, "a tracked pair whose meta is gone is skipped")
	require.Nil(t, viewedMeta(reader, "sess-theirs", otherSupplier), "and so is its meta")
}

// TestWarmupDoesNotOverwriteAPairAlreadyInTheView: the view holds a pair, Redis
// then moves on, and a later warmup keeps the view's value.
func TestWarmupDoesNotOverwriteAPairAlreadyInTheView(t *testing.T) {
	ctx := context.Background()
	writerRedis, prefix := newTestRedis(t)
	writer := newChargeMeterOn(t, writerRedis, true)
	charges := newChargeWriter(t, writer, writerRedis)
	const sessionID = "sess-viewed"
	meterLivePairs(t, writer, charges, chargeTestSupplier, []string{sessionID})

	reader := newChargeMeterOn(t, newTestRedisOnPrefix(t, prefix), false)
	reader.SetDispatcherHeartbeat(time.Now)
	reservation, allowed, err := reader.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed, "premise: the pair is in the view")
	reader.Release(reservation)
	metaBefore := viewedMeta(reader, sessionID, chargeTestSupplier)
	require.NotNil(t, metaBefore, "premise: the pair's meta is in the view")
	key := reader.consumedKey(sessionID, chargeTestSupplier)
	require.NoError(t, writerRedis.Set(ctx, key, 400, time.Hour).Err())

	warmed, err := reader.WarmFromRedis(ctx, signsOnly(chargeTestSupplier))

	require.NoError(t, err)
	require.Zero(t, warmed)
	consumed, ok := viewed(reader, key)
	require.True(t, ok)
	require.Equal(t, int64(1), consumed, "the warmup must not overwrite a value the view already holds")
	require.Same(t, metaBefore, viewedMeta(reader, sessionID, chargeTestSupplier), "nor the meta it already holds")
}

// TestAPairWithNothingChargedYetIsWarmedAtZero: a pair is tracked from its first
// admission, but its counter exists only after its first charge, so a warmup
// that finds the meta and no counter warms the pair at zero.
func TestAPairWithNothingChargedYetIsWarmedAtZero(t *testing.T) {
	ctx := context.Background()
	writerRedis, prefix := newTestRedis(t)
	writer := newChargeMeterOn(t, writerRedis, true)
	newChargeWriter(t, writer, writerRedis)
	const sessionID = "sess-fresh"
	reservation, allowed, err := writer.Admit(ctx, sessionID, chargeTestApp, chargeTestService, chargeTestSupplier, 91, 100, 0)
	require.NoError(t, err)
	require.True(t, allowed)
	writer.Release(reservation)
	key := writer.consumedKey(sessionID, chargeTestSupplier)
	exists, err := writerRedis.Exists(ctx, key).Result()
	require.NoError(t, err)
	require.Zero(t, exists, "premise: nothing was charged, so the pair has no counter")

	reader := newChargeMeterOn(t, newTestRedisOnPrefix(t, prefix), false)
	warmed, err := reader.WarmFromRedis(ctx, signsOnly(chargeTestSupplier))

	require.NoError(t, err)
	require.Equal(t, 1, warmed)
	consumed, ok := viewed(reader, key)
	require.True(t, ok, "a pair with a meta and no counter is warmed")
	require.Zero(t, consumed)
	require.NotNil(t, viewedMeta(reader, sessionID, chargeTestSupplier))
}

var errPipelineLost = errors.New("pipeline lost")

// failPipelines fails every pipeline while single commands still reach Redis,
// which is how a store lost between listing the pairs and reading them looks.
type failPipelines struct{}

func (failPipelines) DialHook(next goredis.DialHook) goredis.DialHook          { return next }
func (failPipelines) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook { return next }
func (failPipelines) ProcessPipelineHook(goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(context.Context, []goredis.Cmder) error { return errPipelineLost }
}

// TestWarmupThatLosesTheStoreWhileReadingThePairsReportsIt: the pairs are listed
// and then the round that reads them fails, so the warmup reports the store as
// unavailable instead of warming nothing and returning no error.
func TestWarmupThatLosesTheStoreWhileReadingThePairsReportsIt(t *testing.T) {
	ctx := context.Background()
	writerRedis, prefix := newTestRedis(t)
	writer := newChargeMeterOn(t, writerRedis, true)
	charges := newChargeWriter(t, writer, writerRedis)
	meterLivePairs(t, writer, charges, chargeTestSupplier, []string{"sess-lost"})

	readerRedis := newTestRedisOnPrefix(t, prefix)
	reader := newChargeMeterOn(t, readerRedis, false)
	readerRedis.AddHook(failPipelines{})
	warmed, err := reader.WarmFromRedis(ctx, signsOnly(chargeTestSupplier))

	require.ErrorIs(t, err, ErrMeterStoreUnavailable)
	require.ErrorIs(t, err, errPipelineLost)
	require.Zero(t, warmed)
	_, ok := viewed(reader, reader.consumedKey("sess-lost", chargeTestSupplier))
	require.False(t, ok)
}

// TestWarmupWithTheStoreDownReturnsAndAdmissionStaysClosed: with Redis
// unreachable the warmup returns an error instead of waiting, and the meter
// still refuses what it cannot budget.
func TestWarmupWithTheStoreDownReturnsAndAdmissionStaysClosed(t *testing.T) {
	const appAddr = "pokt1app"
	meter, breakStore := newFailClosedMeter(t, appAddr)
	breakStore()

	warmCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	warmed, err := meter.WarmFromRedis(warmCtx, func(string) bool { return true })

	require.ErrorIs(t, err, ErrMeterStoreUnavailable)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the warmup must not wait out its deadline")
	require.Zero(t, warmed)
	allowed, err := meter.CheckAndConsumeRelay(context.Background(), "sess-down", appAddr, "svc", "pokt1supplier", 100, 91, 95)
	require.ErrorIs(t, err, ErrMeterStoreUnavailable)
	require.False(t, allowed, "admission stays closed")
}
