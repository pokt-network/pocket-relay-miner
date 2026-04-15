//go:build test

package transport

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAcquireMinedRelayMessage_ReturnsZeroed verifies that a freshly
// acquired message has no stale data. This is the core correctness
// guarantee of the pool — a Release must scrub every field.
func TestAcquireMinedRelayMessage_ReturnsZeroed(t *testing.T) {
	m := AcquireMinedRelayMessage()
	require.NotNil(t, m)

	assert.Empty(t, m.RelayHash)
	assert.Empty(t, m.RelayBytes)
	assert.Zero(t, m.ComputeUnitsPerRelay)
	assert.Empty(t, m.SessionId)
	assert.Zero(t, m.SessionEndHeight)
	assert.Empty(t, m.SupplierOperatorAddress)
	assert.Empty(t, m.ServiceId)
	assert.Empty(t, m.ApplicationAddress)
	assert.Zero(t, m.ArrivalBlockHeight)
	assert.Zero(t, m.PublishedAtUnixNano)
	assert.Zero(t, m.SessionStartHeight)

	ReleaseMinedRelayMessage(m)
}

// TestReleaseMinedRelayMessage_ClearsFields writes every field, releases,
// and then checks that a subsequent Acquire (which may or may not hand
// back the same pointer) never exposes the previous values. Even if the
// pool returns a different instance, the released one must have been
// zeroed before going back so no future Acquire can see the stale data.
func TestReleaseMinedRelayMessage_ClearsFields(t *testing.T) {
	m := AcquireMinedRelayMessage()
	m.RelayHash = []byte("hash")
	m.RelayBytes = []byte("payload")
	m.ComputeUnitsPerRelay = 42
	m.SessionId = "sess"
	m.SessionEndHeight = 100
	m.SupplierOperatorAddress = "pokt1supplier"
	m.ServiceId = "svc"
	m.ApplicationAddress = "pokt1app"
	m.ArrivalBlockHeight = 99
	m.PublishedAtUnixNano = 1234567890
	m.SessionStartHeight = 91

	ReleaseMinedRelayMessage(m)

	// Assert the released pointer itself was scrubbed — defence-in-depth
	// in case a consumer holds a stale reference past Release (which the
	// ownership contract forbids but tests should still catch).
	assert.Empty(t, m.RelayHash)
	assert.Empty(t, m.RelayBytes)
	assert.Zero(t, m.ComputeUnitsPerRelay)
	assert.Empty(t, m.SessionId)
	assert.Empty(t, m.SupplierOperatorAddress)
	assert.Empty(t, m.ServiceId)
	assert.Empty(t, m.ApplicationAddress)
	assert.Zero(t, m.SessionEndHeight)
	assert.Zero(t, m.ArrivalBlockHeight)
	assert.Zero(t, m.PublishedAtUnixNano)
	assert.Zero(t, m.SessionStartHeight)
}

// TestReleaseMinedRelayMessage_NilSafe confirms the release path is a
// no-op on nil, matching the ownership rule "safe to call even when
// upstream code already set the pointer to nil on an error path".
func TestReleaseMinedRelayMessage_NilSafe(t *testing.T) {
	assert.NotPanics(t, func() { ReleaseMinedRelayMessage(nil) })
}

// TestPool_ConcurrentAcquireRelease runs Acquire/Release in parallel
// across many goroutines. With -race this catches any unsafe sharing
// of the pooled struct (the sync.Pool itself is safe; this test guards
// against us accidentally introducing shared mutable state in the
// New or Release paths).
func TestPool_ConcurrentAcquireRelease(t *testing.T) {
	const goroutines = 64
	const iterations = 500

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(gid int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				m := AcquireMinedRelayMessage()
				// Each goroutine writes a unique marker so a leaked
				// cross-goroutine pointer would produce observable
				// corruption when the field is read back.
				m.SessionId = "sess"
				m.ComputeUnitsPerRelay = uint64(gid*iterations + j)
				if m.SessionId != "sess" || m.ComputeUnitsPerRelay == 0 && gid*iterations+j != 0 {
					t.Errorf("unexpected field state after write")
				}
				ReleaseMinedRelayMessage(m)
			}
		}(i)
	}
	wg.Wait()
}

// TestPool_NewInstanceIsDistinctFromReleased is a soft check: the
// sync.Pool docs say Get may return any previously Put item OR a new
// one. We cannot assert "returns the same pointer" because the runtime
// may drop pool contents between GC cycles. We CAN assert that
// consecutive Acquires do not expose stale fields.
func TestPool_NewInstanceIsDistinctFromReleased(t *testing.T) {
	a := AcquireMinedRelayMessage()
	a.SessionId = "leak-marker"
	a.ComputeUnitsPerRelay = 777
	ReleaseMinedRelayMessage(a)

	b := AcquireMinedRelayMessage()
	assert.Empty(t, b.SessionId, "pool must not leak stale string fields")
	assert.Zero(t, b.ComputeUnitsPerRelay, "pool must not leak stale scalar fields")
	ReleaseMinedRelayMessage(b)
}
