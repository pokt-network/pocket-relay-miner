//go:build test

package miner

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// newServiceFactorRegistry builds a registry against a REAL Redis on its own
// namespace. The TTL assertions below are the point of this file, and a fake
// approximates expiry -- which is the one thing that must be exact here.
func newServiceFactorRegistry(t *testing.T, config ServiceFactorRegistryConfig) (*ServiceFactorRegistry, *redisutil.Client) {
	t.Helper()

	client, _ := newTestRedis(t)
	return NewServiceFactorRegistry(zerolog.Nop(), client, client.KB(), config), client
}

// readManifest returns the published manifest, failing if none was written.
func readManifest(t *testing.T, client *redisutil.Client) ServiceFactorManifest {
	t.Helper()

	raw, err := client.Get(context.Background(), client.KB().ServiceFactorManifestKey()).Bytes()
	require.NoError(t, err, "the miner must publish a manifest, even when nothing is configured")

	var manifest ServiceFactorManifest
	require.NoError(t, json.Unmarshal(raw, &manifest))
	return manifest
}

// TestPublishServiceFactors_NothingConfiguredIsPublishedAsData is the defect
// this whole change exists for: an operator who configures no factor used to
// produce NO key, so a relayer could not tell that from a miner that had not
// published yet. The first is legitimate and prices by the protocol formula;
// the second means the relayer is pricing against state nobody wrote.
func TestPublishServiceFactors_NothingConfiguredIsPublishedAsData(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{})

	require.NoError(t, registry.PublishServiceFactors(context.Background()))

	manifest := readManifest(t, client)
	require.False(t, manifest.HasDefault, "no default was configured, and that must be stated, not implied by an absent key")
	require.Empty(t, manifest.Overrides, "no overrides were configured")
	require.NotZero(t, manifest.UpdatedAt)
}

// TestPublishServiceFactors_ManifestHasNoExpiry pins the property that makes an
// absence unambiguous. With a TTL, a key that aged out looks exactly like a key
// nobody wrote, which is the confusion the manifest removes.
func TestPublishServiceFactors_ManifestHasNoExpiry(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		DefaultServiceFactor: 0.01,
		ServiceFactors:       map[string]float64{"eth": 0.02},
	})
	ctx := context.Background()

	require.NoError(t, registry.PublishServiceFactors(ctx))

	for _, key := range []string{
		client.KB().ServiceFactorManifestKey(),
		client.KB().ServiceFactorDefaultKey(),
		client.KB().ServiceFactorServiceKey("eth"),
	} {
		ttl, err := client.TTL(ctx, key).Result()
		require.NoError(t, err)
		require.Equal(t, time.Duration(-1), ttl, "%s must be persistent: -1 is Redis for 'no expiry'", key)
	}
}

// TestPublishServiceFactors_LegacyKeysAreWrittenForOldRelayers holds the
// dual-write. A relayer built before the manifest reads ONLY the per-key
// entries, so a miner that stopped writing them would unprice every such
// relayer the moment it deployed -- which is what the "miner first" commit
// order exists to prevent.
func TestPublishServiceFactors_LegacyKeysAreWrittenForOldRelayers(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		DefaultServiceFactor: 0.01,
		ServiceFactors:       map[string]float64{"eth": 0.02},
	})
	ctx := context.Background()

	require.NoError(t, registry.PublishServiceFactors(ctx))

	var defaultData ServiceFactorData
	raw, err := client.Get(ctx, client.KB().ServiceFactorDefaultKey()).Bytes()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &defaultData))
	require.InDelta(t, 0.01, defaultData.Factor, 1e-9)

	var serviceData ServiceFactorData
	raw, err = client.Get(ctx, client.KB().ServiceFactorServiceKey("eth")).Bytes()
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &serviceData))
	require.InDelta(t, 0.02, serviceData.Factor, 1e-9)
}

// TestPublishServiceFactors_RemovedOverrideDisappearsFromTheManifest is the
// argument the per-key format cannot satisfy: the miner never deletes. An
// override dropped from the config used to stand in Redis until its key
// expired, and a new leader could not clear it because it did not know the key
// existed. Replacing the whole document retires it in one write.
func TestPublishServiceFactors_RemovedOverrideDisappearsFromTheManifest(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		ServiceFactors: map[string]float64{"eth": 0.02, "poly": 0.015},
	})
	ctx := context.Background()

	require.NoError(t, registry.PublishServiceFactors(ctx))
	require.Len(t, readManifest(t, client).Overrides, 2, "premise: both overrides were published")

	// The operator drops one from the config and the miner republishes.
	registry.config.ServiceFactors = map[string]float64{"eth": 0.02}
	require.NoError(t, registry.PublishServiceFactors(ctx))

	manifest := readManifest(t, client)
	require.NotContains(t, manifest.Overrides, "poly", "an override removed from the config must stop being published")
	require.Contains(t, manifest.Overrides, "eth")
}

// TestPublishServiceFactors_InvalidOverrideIsNotPublished keeps a factor that
// would price relays at zero out of the manifest.
func TestPublishServiceFactors_InvalidOverrideIsNotPublished(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		ServiceFactors: map[string]float64{"eth": 0.02, "bad": 0},
	})

	require.NoError(t, registry.PublishServiceFactors(context.Background()))

	manifest := readManifest(t, client)
	require.NotContains(t, manifest.Overrides, "bad", "a factor <= 0 must not reach the manifest")
	require.Contains(t, manifest.Overrides, "eth")
}

// TestStart_RepublishesOnItsInterval proves the auto-healing that replaces the
// TTL. Without a rewrite, a manifest written by a miner that has since lost
// leadership stands forever, because nothing expires it.
func TestStart_RepublishesOnItsInterval(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		DefaultServiceFactor: 0.01,
		RepublishInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()

	require.NoError(t, registry.Start(ctx))
	t.Cleanup(func() { _ = registry.Close() })

	// Delete what the leader wrote and require the loop to put it back.
	key := client.KB().ServiceFactorManifestKey()
	require.NoError(t, client.Del(ctx, key).Err())

	require.Eventually(t, func() bool {
		return client.Exists(ctx, key).Val() == 1
	}, 2*time.Second, 10*time.Millisecond, "the leader must rewrite the manifest on its interval")
}

// TestClose_StopsRepublishing is the other half: the loop must die with the
// registry. The context Start receives belongs to the leader elector and is NOT
// cancelled when leadership is lost, so a loop that ignored Close would keep
// rewriting this miner's config from a miner that no longer leads.
func TestClose_StopsRepublishing(t *testing.T) {
	registry, client := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		DefaultServiceFactor: 0.01,
		RepublishInterval:    20 * time.Millisecond,
	})
	ctx := context.Background()

	require.NoError(t, registry.Start(ctx))
	require.NoError(t, registry.Close())

	key := client.KB().ServiceFactorManifestKey()
	require.NoError(t, client.Del(ctx, key).Err())

	require.Never(t, func() bool {
		return client.Exists(ctx, key).Val() == 1
	}, 500*time.Millisecond, 20*time.Millisecond, "a closed registry must not keep writing")
}

// TestClose_IsIdempotent matches the repository rule that Stop/Close may be
// called twice: cleanup runs on the Start error path and again on Close.
func TestClose_IsIdempotent(t *testing.T) {
	registry, _ := newServiceFactorRegistry(t, ServiceFactorRegistryConfig{
		RepublishInterval: time.Hour,
	})

	require.NoError(t, registry.Start(context.Background()))
	require.NoError(t, registry.Close())
	require.NoError(t, registry.Close())
}
