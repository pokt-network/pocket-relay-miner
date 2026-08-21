package miner

import (
	"context"
	"fmt"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SupplierUpdateAction represents the type of supplier update.
type SupplierUpdateAction string

const (
	SupplierUpdateActionAdd    SupplierUpdateAction = "add"
	SupplierUpdateActionRemove SupplierUpdateAction = "remove"
)

// SupplierRegistryConfig contains configuration for the SupplierRegistry.
type SupplierRegistryConfig struct {

	// IndexKey is the key for the supplier index set.
	// Default: "ha:suppliers:index"
	IndexKey string
}

// SupplierRegistry tracks which suppliers THIS FLEET handles, as a Redis set.
//
// It is membership only: the set answers "does this fleet handle that address",
// and nothing else. The supplier's network state — staked, status, services,
// declared endpoints — is the supplier CACHE's job (ha:supplier:{addr},
// singular), which the relayer reads to decide whether to serve a relay.
type SupplierRegistry struct {
	logger      logging.Logger
	redisClient *redisutil.Client
	config      SupplierRegistryConfig
}

// NewSupplierRegistry creates a new supplier registry.
func NewSupplierRegistry(
	logger logging.Logger,
	redisClient *redisutil.Client,
	config SupplierRegistryConfig,
) *SupplierRegistry {
	if config.IndexKey == "" {
		config.IndexKey = redisClient.KB().SuppliersRegistryIndexKey()
	}

	return &SupplierRegistry{
		logger:      logging.ForComponent(logger, logging.ComponentSupplierRegistry),
		redisClient: redisClient,
		config:      config,
	}
}

// PublishSupplierUpdate adds or removes an address from the fleet's supplier
// index.
//
// It used to also write a per-supplier JSON value at ha:suppliers:{addr}. That
// value had ZERO readers: GetSupplier/GetAllSuppliers had no production callers,
// its Services field was always nil because every call site passed nil, and the
// relayer reads the supplier cache instead — verified by grepping the key's
// LITERAL through relayer/ and miner/, not just the method names, so a reader
// building the key by hand would have shown up too.
//
// Keeping it also kept a latent collision: SupplierStateKey is built from the
// configurable ns.SupplierPrefix while the registry key hardcoded "suppliers",
// so an operator setting supplier_prefix: "suppliers" made the two collide on
// one key with two different structs writing it — and because they shared the
// "status" and "services" JSON fields the cross-read did not even fail, it
// returned a half-populated struct (no "staked" -> IsActive() false -> relays
// refused). Deleting the value removes that by construction instead of by
// validation.
//
// The index is what has readers: balance_monitor (ListSuppliers) and
// KnownSupplierAddresses (orphan-stream detection).
//
// The services argument is accepted and ignored: every caller passes nil, and
// the service list belongs to the cache entry, which derives it from the chain.
func (r *SupplierRegistry) PublishSupplierUpdate(
	ctx context.Context,
	action SupplierUpdateAction,
	operatorAddr string,
	_ []string,
) error {
	switch action {
	case SupplierUpdateActionAdd:
		if err := r.redisClient.SAdd(ctx, r.config.IndexKey, operatorAddr).Err(); err != nil {
			return fmt.Errorf("failed to add to supplier index: %w", err)
		}

	case SupplierUpdateActionRemove:
		if err := r.redisClient.SRem(ctx, r.config.IndexKey, operatorAddr).Err(); err != nil {
			return fmt.Errorf("failed to remove from supplier index: %w", err)
		}

	default:
		// No silent no-op. The switch used to fall through for an unrecognised
		// action: nothing was written, the counter was still incremented with
		// that action as its label, and nil came back -- so the caller believed
		// the registry had been updated and the metric agreed with it. Failing
		// here also keeps the label bounded to the constants above, which is
		// what stops an arbitrary string from becoming a Prometheus series.
		return fmt.Errorf("unknown supplier update action %q", action)
	}

	supplierRegistryUpdatesTotal.WithLabelValues(string(action)).Inc()

	// A membership change is a fleet state change, not a per-request event: it
	// fires once when a supplier joins or leaves THIS fleet, and it is what an
	// operator reads when asking why an address is unmonitored or its streams
	// look orphaned. Until now only the failure was logged, so the successful
	// change -- the one that actually moved the index -- left no trace.
	r.logger.Info().
		Str(logging.FieldSupplier, operatorAddr).
		Str("action", string(action)).
		Msg("fleet supplier index updated")

	return nil
}

// ListSuppliers returns all registered supplier addresses.
func (r *SupplierRegistry) ListSuppliers(ctx context.Context) ([]string, error) {
	suppliers, err := r.redisClient.SMembers(ctx, r.config.IndexKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list suppliers: %w", err)
	}

	return suppliers, nil
}
