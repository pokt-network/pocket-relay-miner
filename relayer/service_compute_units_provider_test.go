//go:build test

package relayer

import (
	"context"
	"errors"
	"testing"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// fakeServiceCache is a minimal ServiceCache stand-in for testing the
// service-cache-backed compute units provider. It returns whatever service
// (or error) it is currently configured with, so tests can mutate the value
// between calls to prove the provider reads the live cache.
type fakeServiceCache struct {
	svc *sharedtypes.Service
	err error
}

func (f *fakeServiceCache) Get(_ context.Context, _ string, _ ...bool) (*sharedtypes.Service, error) {
	return f.svc, f.err
}

// culog returns a logging.Logger for the compute-units provider tests
// (the package's shared testLogger returns a zerolog.Logger instead).
func culog() logging.Logger {
	return logging.NewLoggerFromConfig(logging.DefaultConfig())
}

func TestServiceCacheComputeUnitsProvider_ReturnsCUPRFromCache(t *testing.T) {
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc)

	require.Equal(t, uint64(6312), p.GetServiceComputeUnits("seda"))
}

// TestServiceCacheComputeUnitsProvider_ReflectsRefreshedValue is the core
// regression test for the CUPR-frozen-cache incident: when the service's
// compute_units_per_relay changes on-chain (and the refreshed serviceCache
// picks it up), the provider must return the NEW value without a restart.
func TestServiceCacheComputeUnitsProvider_ReflectsRefreshedValue(t *testing.T) {
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6276}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc)
	require.Equal(t, uint64(6276), p.GetServiceComputeUnits("seda"))

	// On-chain CUPR changes; the orchestrator-refreshed serviceCache now holds 6312.
	fc.svc = &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 6312}
	require.Equal(t, uint64(6312), p.GetServiceComputeUnits("seda"),
		"provider must reflect the refreshed CUPR, not a frozen value")
}

func TestServiceCacheComputeUnitsProvider_DefaultsToOneOnError(t *testing.T) {
	fc := &fakeServiceCache{err: errors.New("service not found")}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc)

	require.Equal(t, uint64(1), p.GetServiceComputeUnits("unknown"))
}

func TestServiceCacheComputeUnitsProvider_DefaultsToOneOnZero(t *testing.T) {
	fc := &fakeServiceCache{svc: &sharedtypes.Service{Id: "seda", ComputeUnitsPerRelay: 0}}
	p := NewServiceCacheComputeUnitsProvider(culog(), fc)

	require.Equal(t, uint64(1), p.GetServiceComputeUnits("seda"),
		"zero CUPR would break claim math; must floor to 1")
}
