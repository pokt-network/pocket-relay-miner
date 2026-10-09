package miner

import (
	"fmt"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// StoreBackend is where a supplier's relays, sessions and dedup marks are kept:
// Redis for the miner subcommand, the embedded store for standalone. The
// supplier manager builds every supplier's stores through it, so the business
// logic above them exists once.
type StoreBackend interface {
	// forSupplier builds the stores of one supplier being added. dedup is the
	// deduplicator the supplier's relays are finished with one at a time; the
	// batch committer marks the same set.
	forSupplier(supplier string, dedup Deduplicator) (supplierStores, error)
	// sessionStore builds a supplier's session store alone, for reads made
	// outside a running supplier (handoff, pending-session checks).
	sessionStore(supplier string) SessionStore
	// deduplicator is the process's one deduplicator; nil fails open.
	deduplicator() Deduplicator
	// smstStore keeps a supplier's session trees.
	smstStore(supplier string) smstStore
	// rebroadcastStore keeps the built claim and proof messages the inclusion
	// reconciler sends again.
	rebroadcastStore() RebroadcastStorage
	// leaseStore holds the supplier leases the claimer shares out.
	leaseStore() leaseStore
}

// supplierStores are one supplier's stores.
type supplierStores struct {
	sessions SessionStore
	consumer transport.MinedRelayConsumer
	// commit finishes relay batches; nil means the supplier finishes every
	// relay on its own (no batch).
	commit relayCommitter
	// setPause holds the consumer's reads while the process admission says so.
	// Called before Consume.
	setPause func(redistransport.IngestionPause)
}

// redisStoreBackend keeps every store in Redis: the miner subcommand's backend.
type redisStoreBackend struct {
	logger logging.Logger
	config SupplierManagerConfig
	dedup  Deduplicator
}

// newRedisStoreBackend builds the backend from the manager's Redis client. With
// no client (tests) there is no deduplicator, which handleRelay fails open on.
func newRedisStoreBackend(logger logging.Logger, config SupplierManagerConfig) *redisStoreBackend {
	b := &redisStoreBackend{logger: logger, config: config}
	if config.RedisClient != nil {
		// BlockTimeSeconds forwarded from config, not left zero: an empty
		// DeduplicatorConfig here used to mean the operator's configured
		// block_time_seconds was silently dropped, and NewRedisDeduplicator's
		// own fallback (30) took over regardless of what was set. On mainnet
		// (verified live 2026-08-21, ~64s/block) that produced a dedup TTL
		// (TTLBlocks=10 x 30s = 5min) roughly HALF the wall-clock window it
		// was meant to cover (~10.7min) -- a relay duplicate arriving after 5
		// minutes but within the intended 10-block window would no longer be
		// caught, and would be counted a second time.
		b.dedup = NewRedisDeduplicator(
			logger,
			config.RedisClient,
			DeduplicatorConfig{BlockTimeSeconds: config.BlockTimeSeconds},
		)
	}
	return b
}

func (b *redisStoreBackend) deduplicator() Deduplicator { return b.dedup }

func (b *redisStoreBackend) sessionStore(supplier string) SessionStore {
	return b.redisSessionStore(supplier)
}

func (b *redisStoreBackend) redisSessionStore(supplier string) *RedisSessionStore {
	return NewRedisSessionStore(b.logger, b.config.RedisClient, SessionStoreConfig{
		SupplierAddress: supplier,
		SessionTTL:      b.config.SessionTTL,
	})
}

func (b *redisStoreBackend) forSupplier(supplier string, dedup Deduplicator) (supplierStores, error) {
	sessions := b.redisSessionStore(supplier)
	// Single stream per supplier; blocks for one block interval per read.
	consumer, err := redistransport.NewStreamsConsumer(
		b.logger,
		b.config.RedisClient,
		transport.ConsumerConfig{
			StreamPrefix:            b.config.RedisClient.KB().StreamPrefix(), // Namespace-aware prefix (e.g., "ha:relays")
			SupplierOperatorAddress: supplier,
			ConsumerGroup:           b.config.RedisClient.KB().ConsumerGroup(), // Namespace-aware group (e.g., "ha-miners")
			ConsumerName:            b.config.ConsumerName,
			BatchSize:               int64(b.config.BatchSize),                // Use config value (default: 1000)
			ClaimIdleTimeout:        b.config.ClaimIdleTimeout.Milliseconds(), // From config (default: 60000ms)
		},
	)
	if err != nil {
		return supplierStores{}, fmt.Errorf("failed to create consumer for %s: %w", supplier, err)
	}
	consumer.SetStoreHealth(b.config.StoreHealth)
	return supplierStores{
		sessions: sessions,
		consumer: consumer,
		commit:   newRedisRelayCommitter(b.config.RedisClient, sessions, dedup, consumer),
		setPause: consumer.SetIngestionPause,
	}, nil
}

func (b *redisStoreBackend) smstStore(supplier string) smstStore {
	return newRedisSMSTStore(b.config.RedisClient, supplier)
}

func (b *redisStoreBackend) rebroadcastStore() RebroadcastStorage {
	return NewRebroadcastStore(b.config.RedisClient, 0) // 0 → default TTL
}

func (b *redisStoreBackend) leaseStore() leaseStore {
	return &redisLeaseStore{client: b.config.RedisClient}
}
