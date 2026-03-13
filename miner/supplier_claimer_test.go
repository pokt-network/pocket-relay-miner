//go:build test

package miner

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/suite"
)

// SupplierClaimerTestSuite tests the SupplierClaimer functionality.
// Uses miniredis for real Redis operations (Rule #1: no mocks).
type SupplierClaimerTestSuite struct {
	suite.Suite
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	ctx         context.Context
}

func (s *SupplierClaimerTestSuite) SetupSuite() {
	mr, err := miniredis.Run()
	s.Require().NoError(err, "failed to create miniredis")
	s.miniRedis = mr
	s.ctx = context.Background()

	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(s.ctx, redisutil.ClientConfig{
		URL: redisURL,
	})
	s.Require().NoError(err, "failed to create Redis client")
	s.redisClient = client
}

func (s *SupplierClaimerTestSuite) SetupTest() {
	s.miniRedis.FlushAll()
}

func (s *SupplierClaimerTestSuite) TearDownSuite() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}

// createTestClaimer creates a SupplierClaimer for testing.
func (s *SupplierClaimerTestSuite) createTestClaimer(instanceID string) *SupplierClaimer {
	logger := zerolog.Nop()
	return NewSupplierClaimer(logger, s.redisClient, instanceID, SupplierClaimerConfig{})
}

// TestReleaseExcess_NewestFirst verifies that releaseExcess releases newest-claimed
// suppliers first (most recently claimed = least established).
// DRAIN-07: When 4 suppliers are claimed at different times (A at t=0, B at t=1,
// C at t=2, D at t=3) and releaseExcess(2) is called, suppliers D and C are
// released (newest first), while A and B remain claimed.
func (s *SupplierClaimerTestSuite) TestReleaseExcess_NewestFirst() {
	claimer := s.createTestClaimer("test-instance-1")
	claimer.ctx, claimer.cancelFn = context.WithCancel(s.ctx)
	defer claimer.cancelFn()

	// Register instance so TryClaim works
	err := claimer.registerInstance(s.ctx)
	s.Require().NoError(err)

	suppliers := []string{"supplierA", "supplierB", "supplierC", "supplierD"}
	claimer.allSuppliers = suppliers

	// Claim all 4 suppliers via TryClaim (sets Redis keys + in-memory map)
	for _, supplier := range suppliers {
		ok := claimer.TryClaim(s.ctx, supplier)
		s.Require().True(ok, "TryClaim should succeed for %s", supplier)
	}
	s.Require().Equal(4, claimer.ClaimedCount())

	// Set deterministic timestamps (A oldest, D newest) -- no time.Sleep needed
	now := time.Now()
	claimer.claimedMu.Lock()
	claimer.claimed["supplierA"] = now.Add(-3 * time.Second) // oldest
	claimer.claimed["supplierB"] = now.Add(-2 * time.Second)
	claimer.claimed["supplierC"] = now.Add(-1 * time.Second)
	claimer.claimed["supplierD"] = now // newest
	claimer.claimedMu.Unlock()

	// Track release order
	var releasedOrder []string
	claimer.onReleaseFn = func(_ context.Context, supplier string) error {
		releasedOrder = append(releasedOrder, supplier)
		return nil
	}

	// Release 2 excess -- should release D (newest) and C (next newest)
	claimer.releaseExcess(2)

	// Verify: D and C released, A and B remain
	s.Require().False(claimer.IsClaimed("supplierD"), "supplierD (newest) should be released")
	s.Require().False(claimer.IsClaimed("supplierC"), "supplierC (second newest) should be released")
	s.Require().True(claimer.IsClaimed("supplierA"), "supplierA (oldest) should remain")
	s.Require().True(claimer.IsClaimed("supplierB"), "supplierB (second oldest) should remain")
	s.Require().Equal(2, claimer.ClaimedCount())

	// Verify release order: newest first
	s.Require().Len(releasedOrder, 2)
	s.Require().Equal("supplierD", releasedOrder[0], "supplierD should be released first (newest)")
	s.Require().Equal("supplierC", releasedOrder[1], "supplierC should be released second")
}

// TestReleaseExcess_NewestFirst_AllReleased verifies that when releaseExcess(N)
// where N >= claimed count, all suppliers are released starting from newest.
func (s *SupplierClaimerTestSuite) TestReleaseExcess_NewestFirst_AllReleased() {
	claimer := s.createTestClaimer("test-instance-2")
	claimer.ctx, claimer.cancelFn = context.WithCancel(s.ctx)
	defer claimer.cancelFn()

	err := claimer.registerInstance(s.ctx)
	s.Require().NoError(err)

	suppliers := []string{"supplierA", "supplierB", "supplierC"}
	claimer.allSuppliers = suppliers

	for _, supplier := range suppliers {
		ok := claimer.TryClaim(s.ctx, supplier)
		s.Require().True(ok, "TryClaim should succeed for %s", supplier)
	}

	// Set deterministic timestamps
	now := time.Now()
	claimer.claimedMu.Lock()
	claimer.claimed["supplierA"] = now.Add(-2 * time.Second) // oldest
	claimer.claimed["supplierB"] = now.Add(-1 * time.Second)
	claimer.claimed["supplierC"] = now // newest
	claimer.claimedMu.Unlock()

	var releasedOrder []string
	claimer.onReleaseFn = func(_ context.Context, supplier string) error {
		releasedOrder = append(releasedOrder, supplier)
		return nil
	}

	// Release all (count >= claimed)
	claimer.releaseExcess(5)

	s.Require().Equal(0, claimer.ClaimedCount(), "all suppliers should be released")
	s.Require().Len(releasedOrder, 3)
	s.Require().Equal("supplierC", releasedOrder[0], "supplierC (newest) should be released first")
	s.Require().Equal("supplierB", releasedOrder[1], "supplierB should be released second")
	s.Require().Equal("supplierA", releasedOrder[2], "supplierA (oldest) should be released last")
}

// TestClaimedMapTimestamp verifies that after TryClaim succeeds, the claimed map
// entry contains a non-zero time.Time value.
func (s *SupplierClaimerTestSuite) TestClaimedMapTimestamp() {
	claimer := s.createTestClaimer("test-instance-3")
	claimer.ctx, claimer.cancelFn = context.WithCancel(s.ctx)
	defer claimer.cancelFn()

	err := claimer.registerInstance(s.ctx)
	s.Require().NoError(err)

	claimer.allSuppliers = []string{"supplierX"}

	before := time.Now()
	ok := claimer.TryClaim(s.ctx, "supplierX")
	s.Require().True(ok, "TryClaim should succeed")
	after := time.Now()

	// Verify the claimed map stores a valid timestamp
	claimer.claimedMu.RLock()
	claimedAt, exists := claimer.claimed["supplierX"]
	claimer.claimedMu.RUnlock()

	s.Require().True(exists, "supplierX should be in claimed map")
	s.Require().False(claimedAt.IsZero(), "claimed timestamp should not be zero")
	s.Require().True(claimedAt.After(before) || claimedAt.Equal(before),
		"claimed timestamp should be >= before time")
	s.Require().True(claimedAt.Before(after) || claimedAt.Equal(after),
		"claimed timestamp should be <= after time")
}

func TestSupplierClaimerTestSuite(t *testing.T) {
	suite.Run(t, new(SupplierClaimerTestSuite))
}
