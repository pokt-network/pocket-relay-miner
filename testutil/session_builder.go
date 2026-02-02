//go:build test

package testutil

import (
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/miner"
)

// SessionBuilder provides a fluent API for building deterministic SessionSnapshot instances.
// The same seed will always produce the same snapshot, ensuring reproducible tests.
//
// Usage:
//
//	snapshot := testutil.NewSessionBuilder(42).
//	    WithSupplier(testutil.TestSupplierAddress()).
//	    WithApp(testutil.TestAppAddress()).
//	    WithService("ethereum").
//	    WithState(miner.SessionStateActive).
//	    Build()
type SessionBuilder struct {
	seed int

	// Configurable fields with defaults derived from seed
	sessionID          *string
	supplierAddr       *string
	appAddr            *string
	serviceID          *string
	state              *miner.SessionState
	sessionStartHeight *int64
	sessionEndHeight   *int64
	relayCount         *int64
	totalComputeUnits  *uint64
	claimedRootHash    []byte
	claimTxHash        *string
	proofTxHash        *string
	lastWALEntryID     *string
	lastUpdatedAt      *time.Time
	createdAt          *time.Time
}

// NewSessionBuilder creates a new SessionBuilder with the given seed.
// All fields will be deterministically generated from this seed unless overridden.
func NewSessionBuilder(seed int) *SessionBuilder {
	return &SessionBuilder{seed: seed}
}

// WithSessionID sets a custom session ID.
func (b *SessionBuilder) WithSessionID(sessionID string) *SessionBuilder {
	b.sessionID = &sessionID
	return b
}

// WithSupplier sets the supplier operator address.
func (b *SessionBuilder) WithSupplier(addr string) *SessionBuilder {
	b.supplierAddr = &addr
	return b
}

// WithApp sets the application address.
func (b *SessionBuilder) WithApp(addr string) *SessionBuilder {
	b.appAddr = &addr
	return b
}

// WithService sets the service ID.
func (b *SessionBuilder) WithService(id string) *SessionBuilder {
	b.serviceID = &id
	return b
}

// WithState sets the session state.
func (b *SessionBuilder) WithState(state miner.SessionState) *SessionBuilder {
	b.state = &state
	return b
}

// WithBlockHeights sets both session start and end heights.
func (b *SessionBuilder) WithBlockHeights(start, end int64) *SessionBuilder {
	b.sessionStartHeight = &start
	b.sessionEndHeight = &end
	return b
}

// WithRelayCount sets the number of relays in the session.
func (b *SessionBuilder) WithRelayCount(count int64) *SessionBuilder {
	b.relayCount = &count
	return b
}

// WithTotalComputeUnits sets the total compute units.
func (b *SessionBuilder) WithTotalComputeUnits(units uint64) *SessionBuilder {
	b.totalComputeUnits = &units
	return b
}

// WithClaimedRootHash sets the claimed root hash.
func (b *SessionBuilder) WithClaimedRootHash(hash []byte) *SessionBuilder {
	b.claimedRootHash = hash
	return b
}

// WithClaimTxHash sets the claim transaction hash.
func (b *SessionBuilder) WithClaimTxHash(hash string) *SessionBuilder {
	b.claimTxHash = &hash
	return b
}

// WithProofTxHash sets the proof transaction hash.
func (b *SessionBuilder) WithProofTxHash(hash string) *SessionBuilder {
	b.proofTxHash = &hash
	return b
}

// WithLastWALEntryID sets the last WAL entry ID.
func (b *SessionBuilder) WithLastWALEntryID(entryID string) *SessionBuilder {
	b.lastWALEntryID = &entryID
	return b
}

// WithTimestamps sets both created and last updated timestamps.
func (b *SessionBuilder) WithTimestamps(created, lastUpdated time.Time) *SessionBuilder {
	b.createdAt = &created
	b.lastUpdatedAt = &lastUpdated
	return b
}

// Build creates a SessionSnapshot with the configured or default values.
// Default values are deterministically generated from the seed.
func (b *SessionBuilder) Build() *miner.SessionSnapshot {
	// Generate deterministic defaults from seed
	sessionID := GenerateDeterministicSessionID(b.seed)
	if b.sessionID != nil {
		sessionID = *b.sessionID
	}

	supplierAddr := TestSupplierAddress()
	if b.supplierAddr != nil {
		supplierAddr = *b.supplierAddr
	}

	appAddr := TestAppAddress()
	if b.appAddr != nil {
		appAddr = *b.appAddr
	}

	serviceID := TestServiceID
	if b.serviceID != nil {
		serviceID = *b.serviceID
	}

	state := miner.SessionStateActive
	if b.state != nil {
		state = *b.state
	}

	// Generate block heights deterministically
	// Use seed to create a base height, then add session length of 4 blocks
	baseHeight := int64(100 + (b.seed * 4))
	sessionStartHeight := baseHeight
	sessionEndHeight := baseHeight + 4
	if b.sessionStartHeight != nil {
		sessionStartHeight = *b.sessionStartHeight
	}
	if b.sessionEndHeight != nil {
		sessionEndHeight = *b.sessionEndHeight
	}

	// Default relay count based on seed
	relayCount := int64(b.seed*10 + 50)
	if b.relayCount != nil {
		relayCount = *b.relayCount
	}

	// Default compute units: 100 per relay
	totalComputeUnits := uint64(relayCount * 100)
	if b.totalComputeUnits != nil {
		totalComputeUnits = *b.totalComputeUnits
	}

	// Generate deterministic timestamps
	// Use a fixed base time to ensure reproducibility
	baseTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	createdAt := baseTime.Add(time.Duration(b.seed) * time.Hour)
	lastUpdatedAt := createdAt.Add(10 * time.Minute)
	if b.createdAt != nil {
		createdAt = *b.createdAt
	}
	if b.lastUpdatedAt != nil {
		lastUpdatedAt = *b.lastUpdatedAt
	}

	snapshot := &miner.SessionSnapshot{
		SessionID:               sessionID,
		SupplierOperatorAddress: supplierAddr,
		ServiceID:               serviceID,
		ApplicationAddress:      appAddr,
		SessionStartHeight:      sessionStartHeight,
		SessionEndHeight:        sessionEndHeight,
		State:                   state,
		RelayCount:              relayCount,
		TotalComputeUnits:       totalComputeUnits,
		CreatedAt:               createdAt,
		LastUpdatedAt:           lastUpdatedAt,
	}

	// Set optional fields if provided
	if b.claimedRootHash != nil {
		snapshot.ClaimedRootHash = b.claimedRootHash
	}
	if b.claimTxHash != nil {
		snapshot.ClaimTxHash = *b.claimTxHash
	}
	if b.proofTxHash != nil {
		snapshot.ProofTxHash = *b.proofTxHash
	}
	if b.lastWALEntryID != nil {
		snapshot.LastWALEntryID = *b.lastWALEntryID
	}

	return snapshot
}

// BuildN creates n SessionSnapshots with incremented seeds.
// Each snapshot has seed = baseSeed + index.
func (b *SessionBuilder) BuildN(n int) []*miner.SessionSnapshot {
	snapshots := make([]*miner.SessionSnapshot, n)
	for i := 0; i < n; i++ {
		// Create a new builder with incremented seed
		builder := NewSessionBuilder(b.seed + i)

		// Copy over any explicit overrides (except session ID)
		if b.supplierAddr != nil {
			builder.supplierAddr = b.supplierAddr
		}
		if b.appAddr != nil {
			builder.appAddr = b.appAddr
		}
		if b.serviceID != nil {
			builder.serviceID = b.serviceID
		}
		if b.state != nil {
			builder.state = b.state
		}

		snapshots[i] = builder.Build()
	}
	return snapshots
}

// String returns a string representation of what would be built.
// Useful for debugging tests.
func (b *SessionBuilder) String() string {
	snapshot := b.Build()
	return fmt.Sprintf("SessionSnapshot{ID: %s, Supplier: %s, App: %s, Service: %s, State: %s, Heights: %d-%d, Relays: %d}",
		snapshot.SessionID,
		snapshot.SupplierOperatorAddress,
		snapshot.ApplicationAddress,
		snapshot.ServiceID,
		snapshot.State,
		snapshot.SessionStartHeight,
		snapshot.SessionEndHeight,
		snapshot.RelayCount,
	)
}
