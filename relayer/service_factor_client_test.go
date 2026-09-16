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

	"github.com/pokt-network/pocket-relay-miner/cache"
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

func (c *getKeyCounter) count(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byKey[key]
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
//
// Start() is deliberately NOT called: it would subscribe to pub/sub and preload
// the default, and these tests are about what GetServiceFactor itself reads.
func newServiceFactorTestClient(t *testing.T) (*ServiceFactorClient, *getKeyCounter) {
	t.Helper()

	redisClient, _ := newTestRedis(t)
	counter := newGetKeyCounter()
	redisClient.AddHook(counter)

	return NewServiceFactorClient(testLogger(), redisClient, DefaultServiceFactorMissingTTL), counter
}

// writeServiceFactor writes a factor under key, the way the miner's registry does.
func writeServiceFactor(t *testing.T, client *ServiceFactorClient, key string, factor float64) {
	t.Helper()

	bz, err := json.Marshal(ServiceFactorData{Factor: factor, UpdatedAt: 1})
	require.NoError(t, err)
	require.NoError(t, client.redisClient.Set(context.Background(), key, bz, 0).Err())
}

// TestGetServiceFactor_AMissingFactorIsReadFromRedisOnce is the defect this
// file exists for: with neither key present, every call used to reach Redis,
// which measured 1.858 GET/s against 1.855 relays/s in a live run. The absence
// must be remembered like a value is.
func TestGetServiceFactor_AMissingFactorIsReadFromRedisOnce(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-absent"

	serviceKey := client.serviceFactorServiceKey(serviceID)
	defaultKey := client.serviceFactorDefaultKey()

	const calls = 5
	for range calls {
		factor, found := client.GetServiceFactor(ctx, serviceID)
		require.False(t, found, "premise: neither key exists, so no factor is configured")
		require.Zero(t, factor)
	}

	require.Equal(t, 1, counter.count(serviceKey),
		"the missing per-service key must be read from Redis once and remembered, not once per call")
	require.Equal(t, 1, counter.count(defaultKey),
		"the missing default key must be read from Redis once and remembered, not once per call")
}

// TestGetServiceFactor_AMissingOverrideStillResolvesTheDefault pins the
// two-level semantics: a remembered absence of the per-service key means "no
// override", not "no factor". It must skip that key's GET and still answer with
// the default.
func TestGetServiceFactor_AMissingOverrideStillResolvesTheDefault(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-no-override"

	serviceKey := client.serviceFactorServiceKey(serviceID)
	defaultKey := client.serviceFactorDefaultKey()
	writeServiceFactor(t, client, defaultKey, 0.25)

	const calls = 5
	for range calls {
		factor, found := client.GetServiceFactor(ctx, serviceID)
		require.True(t, found, "the default must answer for a service with no override")
		require.Equal(t, 0.25, factor)
	}

	require.Equal(t, 1, counter.count(serviceKey),
		"the absent override must be remembered, not re-read on every call")
	require.Equal(t, 1, counter.count(defaultKey),
		"the default is a hit and was already cached; it must be read once")
}

// TestGetServiceFactor_APresentFactorIsUnchanged is the control: the path that
// finds a value must behave exactly as it did before absences were cached.
func TestGetServiceFactor_APresentFactorIsUnchanged(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-present"

	serviceKey := client.serviceFactorServiceKey(serviceID)
	writeServiceFactor(t, client, serviceKey, 0.75)

	const calls = 5
	for range calls {
		factor, found := client.GetServiceFactor(ctx, serviceID)
		require.True(t, found)
		require.Equal(t, 0.75, factor)
	}

	require.Equal(t, 1, counter.count(serviceKey),
		"a present factor is cached on the first read, as it always was")
	require.Zero(t, counter.count(client.serviceFactorDefaultKey()),
		"an override answers the call; the default must not be consulted at all")
}

// TestGetServiceFactor_InvalidationDropsTheRememberedAbsence proves the pub/sub
// handler clears the negative entry too, so a factor published after the
// absence was cached is picked up at once rather than at the TTL.
//
// Step 3 is what keeps this test from passing without the fix: it asserts the
// absence IS being honoured while the key already exists in Redis. Without
// negative caching the client would re-read and find the factor there, and the
// "must still be" assertion fails.
func TestGetServiceFactor_InvalidationDropsTheRememberedAbsence(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-late-factor"
	serviceKey := client.serviceFactorServiceKey(serviceID)

	_, found := client.GetServiceFactor(ctx, serviceID)
	require.False(t, found, "premise: no factor is configured yet")
	require.Equal(t, 1, counter.count(serviceKey))

	writeServiceFactor(t, client, serviceKey, 0.5)

	_, found = client.GetServiceFactor(ctx, serviceID)
	require.False(t, found,
		"the absence must still be honoured before the invalidation arrives")
	require.Equal(t, 1, counter.count(serviceKey),
		"honouring the absence means not going back to Redis")

	payload, err := json.Marshal(cache.ServiceFactorInvalidationPayload{ServiceID: serviceID})
	require.NoError(t, err)
	require.NoError(t, client.handleInvalidation(ctx, string(payload)))

	factor, found := client.GetServiceFactor(ctx, serviceID)
	require.True(t, found,
		"the invalidation must drop the remembered absence, not only a cached value")
	require.Equal(t, 0.5, factor)
	require.Equal(t, 2, counter.count(serviceKey),
		"dropping the absence sends the next call back to Redis")
}

// TestGetServiceFactor_InvalidationDropsTheRememberedAbsenceOfTheDefault is the
// same claim for the default key, whose invalidation payload carries no
// service_id and clears the whole default entry.
func TestGetServiceFactor_InvalidationDropsTheRememberedAbsenceOfTheDefault(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-default-late"
	defaultKey := client.serviceFactorDefaultKey()

	_, found := client.GetServiceFactor(ctx, serviceID)
	require.False(t, found, "premise: no default is configured yet")

	writeServiceFactor(t, client, defaultKey, 0.1)

	_, found = client.GetServiceFactor(ctx, serviceID)
	require.False(t, found,
		"the absent default must still be honoured before the invalidation arrives")
	require.Equal(t, 1, counter.count(defaultKey))

	payload, err := json.Marshal(cache.ServiceFactorInvalidationPayload{})
	require.NoError(t, err)
	require.NoError(t, client.handleInvalidation(ctx, string(payload)))

	factor, found := client.GetServiceFactor(ctx, serviceID)
	require.True(t, found, "the invalidation must drop the remembered absence of the default")
	require.Equal(t, 0.1, factor)
}

// TestGetServiceFactor_TheRememberedAbsenceExpires proves the TTL is real: a
// factor that appears without an invalidation ever arriving is still picked up,
// one TTL later.
//
// The clock is a struct field replaced here, on this goroutine, before any call
// reads it -- the suite forbids time.Sleep, and a package var read inside code
// under test would race.
func TestGetServiceFactor_TheRememberedAbsenceExpires(t *testing.T) {
	client, counter := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-expiring"
	serviceKey := client.serviceFactorServiceKey(serviceID)

	base := time.Now()
	client.now = func() time.Time { return base }

	_, found := client.GetServiceFactor(ctx, serviceID)
	require.False(t, found, "premise: no factor is configured yet")
	require.Equal(t, 1, counter.count(serviceKey))

	// A factor appears, and the invalidation event is lost.
	writeServiceFactor(t, client, serviceKey, 0.9)

	_, found = client.GetServiceFactor(ctx, serviceID)
	require.False(t, found, "within the TTL the absence stands")
	require.Equal(t, 1, counter.count(serviceKey))

	client.now = func() time.Time { return base.Add(DefaultServiceFactorMissingTTL + time.Millisecond) }

	factor, found := client.GetServiceFactor(ctx, serviceID)
	require.True(t, found, "past the TTL the absence must be re-checked against Redis")
	require.Equal(t, 0.9, factor)
	require.Equal(t, 2, counter.count(serviceKey))
}

// TestGetServiceFactor_ATransientRedisErrorIsNotAnAbsence: a timeout or a lost
// connection says nothing about whether the key exists. Remembering it would
// price every relay of this service off one blink of Redis, for a whole TTL.
func TestGetServiceFactor_ATransientRedisErrorIsNotAnAbsence(t *testing.T) {
	client, _ := newServiceFactorTestClient(t)
	ctx := context.Background()
	const serviceID = "svc-redis-blip"
	serviceKey := client.serviceFactorServiceKey(serviceID)

	failRedis := testredis.NewFailSwitch(client.redisClient)
	failRedis.Fail("redis is unreachable")

	_, found := client.GetServiceFactor(ctx, serviceID)
	require.False(t, found, "premise: the failing lookup answers 'not configured'")

	failRedis.Clear()
	writeServiceFactor(t, client, serviceKey, 0.42)

	factor, found := client.GetServiceFactor(ctx, serviceID)
	require.True(t, found,
		"the error must not have been remembered as an absence: the factor is there to be read")
	require.Equal(t, 0.42, factor)
}
