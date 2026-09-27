//go:build test

package relayer

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// The service factor is read once per relay, on the hot path of both validation
// modes. These tests are about how often that read reaches Redis, so they count
// the GETs the client actually issues.

// getKeyCounter counts GET commands per key on the client it is installed on.
//
// The count has to come from a hook on THIS client, never from INFO
// commandstats: the test Redis is shared with every package `go test ./...`
// runs in parallel, so a server-wide counter would also count their traffic and
// the assertion would read as a defect here.
type getKeyCounter struct {
	mu    sync.Mutex
	byKey map[string]int
}

func newGetKeyCounter() *getKeyCounter {
	return &getKeyCounter{byKey: map[string]int{}}
}

// total is what these tests assert on: with the manifest loaded, resolving a
// factor must issue NO Redis command at all, so the interesting number is the
// count across every key rather than one key's.
func (c *getKeyCounter) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	sum := 0
	for _, n := range c.byKey {
		sum += n
	}
	return sum
}

func (c *getKeyCounter) record(cmd goredis.Cmder) {
	if cmd.Name() != "get" || len(cmd.Args()) < 2 {
		return
	}
	key, ok := cmd.Args()[1].(string)
	if !ok {
		return
	}
	c.mu.Lock()
	c.byKey[key]++
	c.mu.Unlock()
}

func (c *getKeyCounter) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (c *getKeyCounter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		c.record(cmd)
		return next(ctx, cmd)
	}
}

func (c *getKeyCounter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		for _, cmd := range cmds {
			c.record(cmd)
		}
		return next(ctx, cmds)
	}
}

// newServiceFactorTestClient returns a client on the shared real Redis, with a
// namespace of its own and a GET counter installed.
func newServiceFactorTestClient(t *testing.T) (*ServiceFactorClient, *getKeyCounter) {
	t.Helper()

	redisClient, _ := newTestRedis(t)
	counter := newGetKeyCounter()
	redisClient.AddHook(counter)

	return NewServiceFactorClient(testLogger(), redisClient), counter
}

// writeManifest publishes a manifest the way the miner's registry does.
func writeManifest(t *testing.T, client *ServiceFactorClient, manifest ServiceFactorManifest) {
	t.Helper()

	bz, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, client.redisClient.Set(
		context.Background(),
		client.redisClient.KB().ServiceFactorManifestKey(),
		bz,
		0,
	).Err())
}

// TestGetServiceFactor_ResolvesWithoutTouchingRedis is the defect this design
// replaces: resolving a factor used to issue a GET per relay, measured at
// 1.858 GET/s against 1.855 relays/s on a live run. The manifest carries the
// whole state, so after the load nothing reaches Redis at all.
func TestGetServiceFactor_ResolvesWithoutTouchingRedis(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{
		HasDefault:    true,
		DefaultFactor: 0.01,
		Overrides:     map[string]float64{"eth": 0.02},
	})
	require.NoError(t, client.loadManifest(ctx))

	gets := counter.total()
	for range 5 {
		factor, found := client.GetServiceFactor(ctx, "eth")
		require.True(t, found)
		require.InDelta(t, 0.02, factor, 1e-9)

		factor, found = client.GetServiceFactor(ctx, "svc-without-override")
		require.True(t, found, "a service with no override takes the default")
		require.InDelta(t, 0.01, factor, 1e-9)
	}

	require.Equal(t, gets, counter.total(),
		"resolving a factor must not reach Redis: the manifest already holds every case")
}

// TestGetServiceFactor_NothingConfiguredIsAPriceNotAnUnknown separates the two
// states the old format could not: an operator who configured no factor is
// PRICED -- the protocol formula applies -- while a relayer with no manifest is
// not, and must refuse.
func TestGetServiceFactor_NothingConfiguredIsAPriceNotAnUnknown(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{Overrides: map[string]float64{}})
	require.NoError(t, client.loadManifest(ctx))

	require.True(t, client.Priced(), "a published manifest is a price, even when it configures nothing")

	factor, found := client.GetServiceFactor(ctx, "any-service")
	require.False(t, found, "no factor configured means the protocol formula, reported as (0, false)")
	require.Zero(t, factor)
}

// TestPriced_IsFalseUntilTheMinerPublishes is the admission gate's premise: with
// no manifest in Redis the relayer does not know what to charge, and a price
// charged wrong is not recoverable.
func TestPriced_IsFalseUntilTheMinerPublishes(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	require.False(t, client.Priced(), "premise: nothing published yet")

	writeManifest(t, client, ServiceFactorManifest{HasDefault: true, DefaultFactor: 0.01})
	require.NoError(t, client.loadManifest(ctx))

	require.True(t, client.Priced(), "the manifest arriving must open admission")
}

// TestStart_RetriesUntilTheManifestAppears proves the relayer waits instead of
// dying: on a cluster restart it can come up before the miner has published.
func TestStart_RetriesUntilTheManifestAppears(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	require.NoError(t, client.Start(ctx), "an absent manifest must not fail startup")
	t.Cleanup(func() { _ = client.Close() })
	require.False(t, client.Priced(), "premise: the miner has not published")

	writeManifest(t, client, ServiceFactorManifest{HasDefault: true, DefaultFactor: 0.05})

	require.Eventually(t, client.Priced, 10*time.Second, 100*time.Millisecond,
		"the retry loop must pick the manifest up without a restart")
}

// TestHandleInvalidation_ReloadsTheWholeManifest keeps a factor change arriving
// without a restart.
func TestHandleInvalidation_ReloadsTheWholeManifest(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{Overrides: map[string]float64{"eth": 0.02}})
	require.NoError(t, client.loadManifest(ctx))

	factor, _ := client.GetServiceFactor(ctx, "eth")
	require.InDelta(t, 0.02, factor, 1e-9, "premise: the first manifest is in force")

	writeManifest(t, client, ServiceFactorManifest{Overrides: map[string]float64{"eth": 0.09}})
	require.NoError(t, client.handleInvalidation(ctx, `{"service_id":"eth"}`))

	factor, found := client.GetServiceFactor(ctx, "eth")
	require.True(t, found)
	require.InDelta(t, 0.09, factor, 1e-9, "the invalidation must replace the manifest, not keep the old value")
}

// TestHandleInvalidation_ARetiredOverrideStopsApplying is the reader's half of
// the miner's delete: the manifest is replaced whole, so an override the
// operator removed stops being applied instead of standing until a key expires.
func TestHandleInvalidation_ARetiredOverrideStopsApplying(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{
		HasDefault:    true,
		DefaultFactor: 0.01,
		Overrides:     map[string]float64{"eth": 0.02},
	})
	require.NoError(t, client.loadManifest(ctx))
	factor, _ := client.GetServiceFactor(ctx, "eth")
	require.InDelta(t, 0.02, factor, 1e-9, "premise: the override applies")

	writeManifest(t, client, ServiceFactorManifest{
		HasDefault:    true,
		DefaultFactor: 0.01,
		Overrides:     map[string]float64{},
	})
	require.NoError(t, client.handleInvalidation(ctx, `{}`))

	factor, found := client.GetServiceFactor(ctx, "eth")
	require.True(t, found)
	require.InDelta(t, 0.01, factor, 1e-9, "with the override retired, the default applies")
}

// TestLoadManifest_ATransientRedisErrorKeepsTheLastGoodManifest is the
// invariant carried over from the per-key client, and it matters MORE here: a
// timeout says nothing about what Redis holds, and treating it as "no price"
// would stop admission across the whole fleet on one blink of Redis -- a worse
// failure than the ambiguity this design removes.
func TestLoadManifest_ATransientRedisErrorKeepsTheLastGoodManifest(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{HasDefault: true, DefaultFactor: 0.01})
	require.NoError(t, client.loadManifest(ctx))
	require.True(t, client.Priced(), "premise: a good manifest is held")

	failRedis := testredis.NewFailSwitch(client.redisClient)
	failRedis.Fail("redis is unreachable")

	require.Error(t, client.loadManifest(ctx), "premise: the reload really failed")

	require.True(t, client.Priced(),
		"a failed reload must not unprice a relayer that already knows what to charge")
	factor, found := client.GetServiceFactor(ctx, "any-service")
	require.True(t, found)
	require.InDelta(t, 0.01, factor, 1e-9, "the last good manifest still answers")

	failRedis.Clear()
}

// TestLoadManifest_AnUnparseableManifestKeepsTheLastGoodOne: a document that
// does not parse is a defect in the producer, not a statement about price.
func TestLoadManifest_AnUnparseableManifestKeepsTheLastGoodOne(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()

	writeManifest(t, client, ServiceFactorManifest{HasDefault: true, DefaultFactor: 0.01})
	require.NoError(t, client.loadManifest(ctx))

	require.NoError(t, client.redisClient.Set(
		ctx, client.redisClient.KB().ServiceFactorManifestKey(), "not json", 0,
	).Err())

	require.Error(t, client.loadManifest(ctx))
	require.True(t, client.Priced(), "garbage from the producer must not unprice this relayer")
	factor, found := client.GetServiceFactor(ctx, "any-service")
	require.True(t, found)
	require.InDelta(t, 0.01, factor, 1e-9)
}
