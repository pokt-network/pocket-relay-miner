package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// SupplierUpdateAction represents the type of supplier update.
type SupplierUpdateAction string

const (
	SupplierUpdateActionAdd      SupplierUpdateAction = "add"
	SupplierUpdateActionUpdate   SupplierUpdateAction = "update"
	SupplierUpdateActionDraining SupplierUpdateAction = "draining"
	SupplierUpdateActionRemove   SupplierUpdateAction = "remove"
)

// SupplierRegistryData is the data stored in Redis for each supplier.
type SupplierRegistryData struct {
	OperatorAddr string   `json:"operator_addr"`
	Services     []string `json:"services"`
	Status       string   `json:"status"` // "active", "draining"
	UpdatedAt    int64    `json:"updated_at"`
}

// SupplierRegistryConfig contains configuration for the SupplierRegistry.
type SupplierRegistryConfig struct {

	// IndexKey is the key for the supplier index set.
	// Default: "ha:suppliers:index"
	IndexKey string
}

// SupplierRegistry manages supplier registration in Redis.
// It allows relayers to discover available suppliers and their services.
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

// PublishSupplierUpdate updates the supplier's registry entry in Redis.
// The name is historical: it used to also publish to a pub/sub channel that
// never had a subscriber anywhere in the fleet; the write to the registry
// keys (read by the CLI and the relayer's supplier discovery) is the real
// mechanism, and the phantom channel was removed.
func (r *SupplierRegistry) PublishSupplierUpdate(
	ctx context.Context,
	action SupplierUpdateAction,
	operatorAddr string,
	services []string,
) error {
	key := r.redisClient.KB().SupplierRegistryKey(operatorAddr)

	switch action {
	case SupplierUpdateActionAdd, SupplierUpdateActionUpdate:
		// Set supplier data
		data := SupplierRegistryData{
			OperatorAddr: operatorAddr,
			Services:     services,
			Status:       "active",
			UpdatedAt:    time.Now().Unix(),
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal supplier data: %w", err)
		}

		if err := r.redisClient.Set(ctx, key, jsonData, 0).Err(); err != nil {
			return fmt.Errorf("failed to set supplier data: %w", err)
		}

		// Add to index
		if err := r.redisClient.SAdd(ctx, r.config.IndexKey, operatorAddr).Err(); err != nil {
			return fmt.Errorf("failed to add to supplier index: %w", err)
		}

	case SupplierUpdateActionDraining:
		// Update status to draining
		data := SupplierRegistryData{
			OperatorAddr: operatorAddr,
			Services:     services,
			Status:       "draining",
			UpdatedAt:    time.Now().Unix(),
		}
		jsonData, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal supplier data: %w", err)
		}

		if err := r.redisClient.Set(ctx, key, jsonData, 0).Err(); err != nil {
			return fmt.Errorf("failed to set supplier data: %w", err)
		}

	case SupplierUpdateActionRemove:
		// Remove supplier data
		if err := r.redisClient.Del(ctx, key).Err(); err != nil {
			return fmt.Errorf("failed to delete supplier data: %w", err)
		}

		// Remove from index
		if err := r.redisClient.SRem(ctx, r.config.IndexKey, operatorAddr).Err(); err != nil {
			return fmt.Errorf("failed to remove from supplier index: %w", err)
		}
	}

	supplierRegistryUpdatesTotal.WithLabelValues(string(action)).Inc()

	return nil
}

// GetSupplier retrieves supplier data from Redis.
func (r *SupplierRegistry) GetSupplier(ctx context.Context, operatorAddr string) (*SupplierRegistryData, error) {
	key := r.redisClient.KB().SupplierRegistryKey(operatorAddr)

	data, err := r.redisClient.Get(ctx, key).Bytes()
	if err != nil {
		if err == redis.Nil {
			return nil, nil // Not found
		}
		return nil, fmt.Errorf("failed to get supplier data: %w", err)
	}

	var supplierData SupplierRegistryData
	if err := json.Unmarshal(data, &supplierData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal supplier data: %w", err)
	}

	return &supplierData, nil
}

// ListSuppliers returns all registered supplier addresses.
func (r *SupplierRegistry) ListSuppliers(ctx context.Context) ([]string, error) {
	suppliers, err := r.redisClient.SMembers(ctx, r.config.IndexKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to list suppliers: %w", err)
	}

	return suppliers, nil
}

// GetAllSuppliers retrieves data for all registered suppliers.
func (r *SupplierRegistry) GetAllSuppliers(ctx context.Context) (map[string]*SupplierRegistryData, error) {
	suppliers, err := r.ListSuppliers(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]*SupplierRegistryData)
	for _, addr := range suppliers {
		data, err := r.GetSupplier(ctx, addr)
		if err != nil {
			r.logger.Warn().
				Err(err).
				Str("operator", addr).
				Msg("failed to get supplier data")
			continue
		}
		if data != nil {
			result[addr] = data
		}
	}

	return result, nil
}

// ClearAll removes all supplier data from Redis.
// Used primarily for testing.
func (r *SupplierRegistry) ClearAll(ctx context.Context) error {
	suppliers, err := r.ListSuppliers(ctx)
	if err != nil {
		return err
	}

	for _, addr := range suppliers {
		key := r.redisClient.KB().SupplierRegistryKey(addr)
		r.redisClient.Del(ctx, key)
	}

	r.redisClient.Del(ctx, r.config.IndexKey)

	return nil
}
