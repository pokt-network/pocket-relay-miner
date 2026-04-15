package cache

// ServiceFactorCacheType is the cache type name used for the
// service_factor pub/sub invalidation channel.
//
// Final channel key: {namespace}:events:cache:service_factor:invalidate
// Used by:
//   - miner/service_factor_registry.go (publisher) when PublishServiceFactors writes new values
//   - relayer/service_factor_client.go (subscriber) to invalidate its L1 cache
const ServiceFactorCacheType = "service_factor"

// ServiceFactorInvalidationPayload is the JSON payload sent on the
// service_factor invalidation channel.
//
// Semantics:
//   - ServiceID == "" → invalidate the default service_factor L1 entry
//   - ServiceID != "" → invalidate the L1 entry for that specific service
//
// The payload is serialized as JSON by the publisher and deserialized
// by the subscriber's handler.
type ServiceFactorInvalidationPayload struct {
	ServiceID string `json:"service_id,omitempty"`
}
