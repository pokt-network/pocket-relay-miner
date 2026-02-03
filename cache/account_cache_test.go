//go:build test

package cache

import (
	"context"
	"errors"
	"sync"
	"testing"

	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/testutil"
	"github.com/stretchr/testify/suite"
)

// mockAccountQueryClient is a local mock implementing AccountQueryClient.
type mockAccountQueryClient struct {
	mu      sync.Mutex
	pubKeys map[string]cryptotypes.PubKey
	err     error
}

func newMockAccountQueryClient() *mockAccountQueryClient {
	return &mockAccountQueryClient{
		pubKeys: make(map[string]cryptotypes.PubKey),
	}
}

func (m *mockAccountQueryClient) GetPubKeyFromAddress(ctx context.Context, address string) (cryptotypes.PubKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	pubKey, ok := m.pubKeys[address]
	if !ok {
		return nil, errors.New("public key not found")
	}

	return pubKey, nil
}

func (m *mockAccountQueryClient) setPubKey(address string, pubKey cryptotypes.PubKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pubKeys[address] = pubKey
}

// AccountCacheTestSuite tests the accountCache implementation.
type AccountCacheTestSuite struct {
	testutil.RedisTestSuite

	mockQueryClient *mockAccountQueryClient
	cache           KeyedEntityCache[string, cryptotypes.PubKey]
	testAddress     string
	testPubKey      cryptotypes.PubKey
}

func TestAccountCacheTestSuite(t *testing.T) {
	suite.Run(t, new(AccountCacheTestSuite))
}

// SetupTest runs before each test.
func (s *AccountCacheTestSuite) SetupTest() {
	s.RedisTestSuite.SetupTest()

	// Create mock query client
	s.mockQueryClient = newMockAccountQueryClient()

	// Create test data
	s.testAddress = testutil.TestAppAddress()
	s.testPubKey = testutil.TestAppPubKey()

	// Pre-populate mock
	s.mockQueryClient.setPubKey(s.testAddress, s.testPubKey)

	// Create cache
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	s.cache = NewAccountCache(
		logger,
		s.RedisClient,
		s.mockQueryClient,
	)

	// Start cache
	err := s.cache.Start(s.Ctx)
	s.Require().NoError(err)
}

// TearDownTest runs after each test.
func (s *AccountCacheTestSuite) TearDownTest() {
	if s.cache != nil {
		s.cache.Close()
	}
}

// TestAccountCache_GetFromL3 verifies cold cache fetches from L3.
func (s *AccountCacheTestSuite) TestAccountCache_GetFromL3() {
	// Cold cache
	s.Require().Equal(0, s.GetKeyCount())

	// Get pub key
	pubKey, err := s.cache.Get(s.Ctx, s.testAddress)
	s.Require().NoError(err)
	s.Require().NotNil(pubKey)
	s.Require().Equal(s.testPubKey.Bytes(), pubKey.Bytes())

	// Verify stored in Redis
	redisKey := s.RedisClient.KB().CacheKey(accountCacheType, s.testAddress)
	s.RequireKeyExists(redisKey)
}

// TestAccountCache_GetFromL1 verifies L1 cache hit.
func (s *AccountCacheTestSuite) TestAccountCache_GetFromL1() {
	// Pre-populate
	pubKey1, err := s.cache.Get(s.Ctx, s.testAddress)
	s.Require().NoError(err)

	// Second get should hit L1
	pubKey2, err := s.cache.Get(s.Ctx, s.testAddress)
	s.Require().NoError(err)
	s.Require().Equal(pubKey1.Bytes(), pubKey2.Bytes())
}

// TestAccountCache_Invalidate verifies invalidation clears cache.
func (s *AccountCacheTestSuite) TestAccountCache_Invalidate() {
	// Pre-populate
	_, err := s.cache.Get(s.Ctx, s.testAddress)
	s.Require().NoError(err)

	// Invalidate
	err = s.cache.Invalidate(s.Ctx, s.testAddress)
	s.Require().NoError(err)

	// Verify cleared from Redis
	redisKey := s.RedisClient.KB().CacheKey(accountCacheType, s.testAddress)
	s.RequireKeyNotExists(redisKey)
}

// TestAccountCache_Set verifies explicit Set works.
func (s *AccountCacheTestSuite) TestAccountCache_Set() {
	// Set pub key
	err := s.cache.Set(s.Ctx, s.testAddress, s.testPubKey, 0)
	s.Require().NoError(err)

	// Verify in Redis
	redisKey := s.RedisClient.KB().CacheKey(accountCacheType, s.testAddress)
	s.RequireKeyExists(redisKey)

	// Get should return from L1
	pubKey, err := s.cache.Get(s.Ctx, s.testAddress)
	s.Require().NoError(err)
	s.Require().Equal(s.testPubKey.Bytes(), pubKey.Bytes())
}
