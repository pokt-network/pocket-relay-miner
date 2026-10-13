//go:build test

package cache

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
)

// These tests prove the `{}` clear-all fix in handleInvalidation for the four
// entity caches (application, service, account, supplier). Before the fix the
// `payload == "{}"` check lived inside the json.Unmarshal error branch, which
// was unreachable ("{}" unmarshals successfully), so a bulk clear-all silently
// did nothing. The handlers now check `payload == "{}"` BEFORE unmarshalling
// and call localCache.Clear().
//
// Each cache is covered by three scenarios:
//  1. clear-all ("{}") empties L1 entirely;
//  2. a targeted payload deletes ONLY the named entry (regression guard so the
//     clear-all branch does not swallow the normal single-key path);
//  3. a malformed payload returns an error and leaves L1 untouched.
//
// handleInvalidation is called directly (internal test), and Start() is
// deliberately NOT called: it would spin up the real pub/sub subscriber, and
// the SupplierCache warm path (SetSupplierState) publishes an invalidation
// event that a live subscriber would consume and race against these direct
// calls. Without Start there is no subscriber, so L1 warming is deterministic.

func testLogger() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}

func TestApplicationCache_HandleInvalidation_ClearAll(t *testing.T) {
	ctx := context.Background()
	const addrA, addrB = "pokt1appA", "pokt1appB"

	newWarmCache := func(t *testing.T) *applicationCache {
		t.Helper()
		client := newTestRedis(t)
		ac := NewApplicationCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
			&frozenApplicationQueryClient{chainDelegatees: []string{"gw-a"}}).(*applicationCache)
		// Public warm path: Get lazy-loads via the query stub and stores into L1.
		_, err := ac.Get(ctx, addrA)
		require.NoError(t, err)
		_, err = ac.Get(ctx, addrB)
		require.NoError(t, err)
		require.Equal(t, 2, ac.localCache.Size(), "precondition: both entries warm in L1")
		return ac
	}

	t.Run("clear-all empties L1", func(t *testing.T) {
		ac := newWarmCache(t)

		require.NoError(t, ac.handleInvalidation(ctx, "{}"))

		assert.Equal(t, 0, ac.localCache.Size(), "clear-all must empty L1")
		_, ok := ac.localCache.Load(addrA)
		assert.False(t, ok, "%s must be evicted by clear-all", addrA)
		_, ok = ac.localCache.Load(addrB)
		assert.False(t, ok, "%s must be evicted by clear-all", addrB)
	})

	t.Run("targeted deletes only that entry", func(t *testing.T) {
		ac := newWarmCache(t)
		// Drop addrA's L2 key so the handler's eager L2->L1 reload cannot
		// immediately re-populate the entry we are asserting was deleted.
		require.NoError(t, ac.store.(*kv.Redis).Client().Del(ctx, ac.store.(*kv.Redis).Client().KB().CacheKey(applicationCacheType, addrA)).Err())

		require.NoError(t, ac.handleInvalidation(ctx, `{"address":"pokt1appA"}`))

		_, ok := ac.localCache.Load(addrA)
		assert.False(t, ok, "targeted invalidation must delete %s", addrA)
		_, ok = ac.localCache.Load(addrB)
		assert.True(t, ok, "targeted invalidation must NOT touch %s", addrB)
		assert.Equal(t, 1, ac.localCache.Size())
	})

	t.Run("malformed payload errors and preserves L1", func(t *testing.T) {
		ac := newWarmCache(t)

		err := ac.handleInvalidation(ctx, "not-json")
		require.Error(t, err)

		assert.Equal(t, 2, ac.localCache.Size(), "malformed payload must not touch L1")
		_, ok := ac.localCache.Load(addrA)
		assert.True(t, ok)
		_, ok = ac.localCache.Load(addrB)
		assert.True(t, ok)
	})
}

func TestServiceCache_HandleInvalidation_ClearAll(t *testing.T) {
	ctx := context.Background()
	const svcA, svcB = "svcA", "svcB"

	newWarmCache := func(t *testing.T) *serviceCache {
		t.Helper()
		client := newTestRedis(t)
		sc := NewServiceCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
			&frozenServiceQueryClient{chainCUPR: 1000}).(*serviceCache)
		_, err := sc.Get(ctx, svcA)
		require.NoError(t, err)
		_, err = sc.Get(ctx, svcB)
		require.NoError(t, err)
		require.Equal(t, 2, sc.localCache.Size(), "precondition: both entries warm in L1")
		return sc
	}

	t.Run("clear-all empties L1", func(t *testing.T) {
		sc := newWarmCache(t)

		require.NoError(t, sc.handleInvalidation(ctx, "{}"))

		assert.Equal(t, 0, sc.localCache.Size(), "clear-all must empty L1")
		_, ok := sc.localCache.Load(svcA)
		assert.False(t, ok, "%s must be evicted by clear-all", svcA)
		_, ok = sc.localCache.Load(svcB)
		assert.False(t, ok, "%s must be evicted by clear-all", svcB)
	})

	t.Run("targeted deletes only that entry", func(t *testing.T) {
		sc := newWarmCache(t)
		// Drop svcA's L2 key so the eager L2->L1 reload cannot re-populate it.
		require.NoError(t, sc.store.(*kv.Redis).Client().Del(ctx, sc.store.(*kv.Redis).Client().KB().CacheKey(serviceCacheType, svcA)).Err())

		require.NoError(t, sc.handleInvalidation(ctx, `{"service_id":"svcA"}`))

		_, ok := sc.localCache.Load(svcA)
		assert.False(t, ok, "targeted invalidation must delete %s", svcA)
		_, ok = sc.localCache.Load(svcB)
		assert.True(t, ok, "targeted invalidation must NOT touch %s", svcB)
		assert.Equal(t, 1, sc.localCache.Size())
	})

	t.Run("malformed payload errors and preserves L1", func(t *testing.T) {
		sc := newWarmCache(t)

		err := sc.handleInvalidation(ctx, "not-json")
		require.Error(t, err)

		assert.Equal(t, 2, sc.localCache.Size(), "malformed payload must not touch L1")
		_, ok := sc.localCache.Load(svcA)
		assert.True(t, ok)
		_, ok = sc.localCache.Load(svcB)
		assert.True(t, ok)
	})
}

func TestAccountCache_HandleInvalidation_ClearAll(t *testing.T) {
	ctx := context.Background()
	const addrA, addrB = "pokt1acctA", "pokt1acctB"

	newWarmCache := func(t *testing.T) *accountCache {
		t.Helper()
		client := newTestRedis(t)
		ac := NewAccountCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
			&frozenAccountQueryClient{pubKey: pubKeyFromByte(0x11)}).(*accountCache)
		_, err := ac.Get(ctx, addrA)
		require.NoError(t, err)
		_, err = ac.Get(ctx, addrB)
		require.NoError(t, err)
		require.Equal(t, 2, ac.localCache.Size(), "precondition: both entries warm in L1")
		return ac
	}

	t.Run("clear-all empties L1", func(t *testing.T) {
		ac := newWarmCache(t)

		require.NoError(t, ac.handleInvalidation(ctx, "{}"))

		assert.Equal(t, 0, ac.localCache.Size(), "clear-all must empty L1")
		_, ok := ac.localCache.Load(addrA)
		assert.False(t, ok, "%s must be evicted by clear-all", addrA)
		_, ok = ac.localCache.Load(addrB)
		assert.False(t, ok, "%s must be evicted by clear-all", addrB)
	})

	t.Run("targeted deletes only that entry", func(t *testing.T) {
		ac := newWarmCache(t)
		// accountCache.handleInvalidation has no eager reload, so no L2 delete needed.

		require.NoError(t, ac.handleInvalidation(ctx, `{"address":"pokt1acctA"}`))

		_, ok := ac.localCache.Load(addrA)
		assert.False(t, ok, "targeted invalidation must delete %s", addrA)
		_, ok = ac.localCache.Load(addrB)
		assert.True(t, ok, "targeted invalidation must NOT touch %s", addrB)
		assert.Equal(t, 1, ac.localCache.Size())
	})

	t.Run("malformed payload errors and preserves L1", func(t *testing.T) {
		ac := newWarmCache(t)

		err := ac.handleInvalidation(ctx, "not-json")
		require.Error(t, err)

		assert.Equal(t, 2, ac.localCache.Size(), "malformed payload must not touch L1")
		_, ok := ac.localCache.Load(addrA)
		assert.True(t, ok)
		_, ok = ac.localCache.Load(addrB)
		assert.True(t, ok)
	})
}

func TestSupplierCache_HandleInvalidation_ClearAll(t *testing.T) {
	ctx := context.Background()
	const opA, opB = "pokt1supA", "pokt1supB"

	healthyState := func(op string) *SupplierState {
		return &SupplierState{
			Staked:          true,
			Status:          SupplierStatusActive,
			OperatorAddress: op,
			Services:        []string{"svc1"},
		}
	}

	newWarmCache := func(t *testing.T) *SupplierCache {
		t.Helper()
		client := newTestRedis(t)
		sc := NewSupplierCache(testLogger(), kv.NewRedis(zerolog.Nop(), client), SupplierCacheConfig{})
		// Public warm path: SetSupplierState stores into L1 (and L2). No live
		// subscriber exists (Start not called), so its published invalidation
		// event is a no-op and cannot race the L1 we just warmed.
		require.NoError(t, sc.SetSupplierState(ctx, healthyState(opA)))
		require.NoError(t, sc.SetSupplierState(ctx, healthyState(opB)))
		require.Equal(t, 2, sc.localCache.Size(), "precondition: both entries warm in L1")
		return sc
	}

	t.Run("clear-all empties L1", func(t *testing.T) {
		sc := newWarmCache(t)

		require.NoError(t, sc.handleInvalidation(ctx, "{}"))

		assert.Equal(t, 0, sc.localCache.Size(), "clear-all must empty L1")
		_, ok := sc.localCache.Load(opA)
		assert.False(t, ok, "%s must be evicted by clear-all", opA)
		_, ok = sc.localCache.Load(opB)
		assert.False(t, ok, "%s must be evicted by clear-all", opB)
	})

	t.Run("targeted deletes only that entry", func(t *testing.T) {
		sc := newWarmCache(t)
		// SupplierCache.handleInvalidation has no eager reload, so no L2 delete needed.

		require.NoError(t, sc.handleInvalidation(ctx, `{"operator_address":"pokt1supA"}`))

		_, ok := sc.localCache.Load(opA)
		assert.False(t, ok, "targeted invalidation must delete %s", opA)
		_, ok = sc.localCache.Load(opB)
		assert.True(t, ok, "targeted invalidation must NOT touch %s", opB)
		assert.Equal(t, 1, sc.localCache.Size())
	})

	t.Run("malformed payload errors and preserves L1", func(t *testing.T) {
		sc := newWarmCache(t)

		err := sc.handleInvalidation(ctx, "not-json")
		require.Error(t, err)

		assert.Equal(t, 2, sc.localCache.Size(), "malformed payload must not touch L1")
		_, ok := sc.localCache.Load(opA)
		assert.True(t, ok)
		_, ok = sc.localCache.Load(opB)
		assert.True(t, ok)
	})
}

// TestHandleInvalidation_ClearAllNegativeControls locks the clear-all trigger
// to the EXACT literal "{}". Valid JSON that merely lacks (or empties) the
// targeted field — a typo'd field name, an empty value, or a whitespace
// variant of the empty object — must NOT wipe L1: a refactor that widened the
// clear-all condition to "no field matched" would turn every malformed-but-
// valid event into a fleet-wide L1 wipe.
func TestHandleInvalidation_ClearAllNegativeControls(t *testing.T) {
	ctx := context.Background()

	payloads := []struct {
		name    string
		payload string
	}{
		{"empty field value", `{"address":"","service_id":"","operator_address":""}`},
		{"unknown field", `{"unknown_field":"x"}`},
		{"whitespace empty object", "{ }"},
	}

	assertSurvives := func(t *testing.T, name string, size int) {
		t.Helper()
		assert.Equalf(t, 2, size, "%s must NOT clear L1 (only the literal {} may)", name)
	}

	for _, tc := range payloads {
		t.Run("application/"+tc.name, func(t *testing.T) {
			client := newTestRedis(t)
			ac := NewApplicationCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
				&frozenApplicationQueryClient{chainDelegatees: []string{"gw-a"}}).(*applicationCache)
			_, err := ac.Get(ctx, "pokt1appA")
			require.NoError(t, err)
			_, err = ac.Get(ctx, "pokt1appB")
			require.NoError(t, err)

			require.NoError(t, ac.handleInvalidation(ctx, tc.payload))
			assertSurvives(t, tc.payload, ac.localCache.Size())
		})

		t.Run("service/"+tc.name, func(t *testing.T) {
			client := newTestRedis(t)
			sc := NewServiceCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
				&frozenServiceQueryClient{chainCUPR: 1000}).(*serviceCache)
			_, err := sc.Get(ctx, "svcA")
			require.NoError(t, err)
			_, err = sc.Get(ctx, "svcB")
			require.NoError(t, err)

			require.NoError(t, sc.handleInvalidation(ctx, tc.payload))
			assertSurvives(t, tc.payload, sc.localCache.Size())
		})

		t.Run("account/"+tc.name, func(t *testing.T) {
			client := newTestRedis(t)
			ac := NewAccountCache(testLogger(), kv.NewRedis(zerolog.Nop(), client),
				&frozenAccountQueryClient{pubKey: pubKeyFromByte(0x11)}).(*accountCache)
			_, err := ac.Get(ctx, "pokt1acctA")
			require.NoError(t, err)
			_, err = ac.Get(ctx, "pokt1acctB")
			require.NoError(t, err)

			require.NoError(t, ac.handleInvalidation(ctx, tc.payload))
			assertSurvives(t, tc.payload, ac.localCache.Size())
		})

		t.Run("supplier/"+tc.name, func(t *testing.T) {
			client := newTestRedis(t)
			sc := NewSupplierCache(testLogger(), kv.NewRedis(zerolog.Nop(), client), SupplierCacheConfig{})
			require.NoError(t, sc.SetSupplierState(ctx, &SupplierState{
				Status: SupplierStatusActive, Staked: true,
				OperatorAddress: "pokt1opA", Services: []string{"svc1"},
			}))
			require.NoError(t, sc.SetSupplierState(ctx, &SupplierState{
				Status: SupplierStatusActive, Staked: true,
				OperatorAddress: "pokt1opB", Services: []string{"svc1"},
			}))

			require.NoError(t, sc.handleInvalidation(ctx, tc.payload))
			assertSurvives(t, tc.payload, sc.localCache.Size())
		})
	}
}
