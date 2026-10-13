//go:build test

package miner

import (
	"context"

	"github.com/pokt-network/smt/kvstore"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// The constructors below build Redis-backed pieces directly, as tests do;
// production builds them through the store backend.

// NewRedisSMSTManager creates a new Redis-backed SMST manager.
// The manager stores SMST nodes in Redis, enabling shared storage across HA instances.
func NewRedisSMSTManager(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config RedisSMSTManagerConfig,
) *RedisSMSTManager {
	return newSMSTManager(logger, newRedisSMSTStore(redisClient, config.SupplierAddress), config)
}

// NewRedisMapStore creates a new Redis-backed MapStore for a (supplier, session) pair.
// The store uses a Redis hash to persist SMST nodes, enabling shared access across HA instances.
//
// Parameters:
//   - ctx: Context for Redis operations
//   - redisClient: Redis client (supports standalone, sentinel, and cluster)
//   - supplierAddress: Supplier operator address — required to namespace the hash per
//     supplier so distinct suppliers participating in the same session do not
//     overwrite each other's SMST nodes.
//   - sessionID: Unique session identifier used to namespace the Redis hash
//
// Returns:
//
//	A MapStore implementation backed by Redis
func NewRedisMapStore(
	ctx context.Context,
	redisClient *redisutil.Client,
	supplierAddress string,
	sessionID string,
) kvstore.MapStore {
	return newRedisMapStore(ctx, redisClient, supplierAddress, sessionID)
}

// NewSupplierClaimer creates a new supplier claimer.
// Uses the provided config values. Zero values fall back to the package-level
// constants (ClaimTTL=90s, RenewRate=10s, etc.).
func NewSupplierClaimer(
	logger logging.Logger,
	redisClient *redisutil.Client,
	instanceID string,
	cfg SupplierClaimerConfig,
) *SupplierClaimer {
	return newSupplierClaimer(logger, &redisLeaseStore{client: redisClient}, instanceID, cfg)
}
