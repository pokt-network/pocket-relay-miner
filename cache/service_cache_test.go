//go:build test

package cache

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/testutil"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	"github.com/stretchr/testify/suite"
)

// mockServiceQueryClient is a local mock implementing ServiceQueryClient.
type mockServiceQueryClient struct {
	mu       sync.Mutex
	services map[string]*sharedtypes.Service
	err      error
}

func newMockServiceQueryClient() *mockServiceQueryClient {
	return &mockServiceQueryClient{
		services: make(map[string]*sharedtypes.Service),
	}
}

func (m *mockServiceQueryClient) GetService(ctx context.Context, serviceID string) (*sharedtypes.Service, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return nil, m.err
	}

	svc, ok := m.services[serviceID]
	if !ok {
		return nil, errors.New("service not found")
	}

	return svc, nil
}

func (m *mockServiceQueryClient) setService(serviceID string, svc *sharedtypes.Service) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.services[serviceID] = svc
}

// ServiceCacheTestSuite tests the serviceCache implementation.
type ServiceCacheTestSuite struct {
	testutil.RedisTestSuite

	mockQueryClient *mockServiceQueryClient
	cache           KeyedEntityCache[string, *sharedtypes.Service]
	testService     *sharedtypes.Service
}

func TestServiceCacheTestSuite(t *testing.T) {
	suite.Run(t, new(ServiceCacheTestSuite))
}

// SetupTest runs before each test.
func (s *ServiceCacheTestSuite) SetupTest() {
	s.RedisTestSuite.SetupTest()

	// Create mock query client
	s.mockQueryClient = newMockServiceQueryClient()

	// Create test service
	s.testService = &sharedtypes.Service{
		Id:   testutil.TestServiceID,
		Name: "Test Service",
	}

	// Pre-populate mock
	s.mockQueryClient.setService(s.testService.Id, s.testService)

	// Create cache
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	s.cache = NewServiceCache(
		logger,
		s.RedisClient,
		s.mockQueryClient,
	)

	// Start cache
	err := s.cache.Start(s.Ctx)
	s.Require().NoError(err)
}

// TearDownTest runs after each test.
func (s *ServiceCacheTestSuite) TearDownTest() {
	if s.cache != nil {
		s.cache.Close()
	}
}

// TestServiceCache_GetFromL3 verifies cold cache fetches from L3.
func (s *ServiceCacheTestSuite) TestServiceCache_GetFromL3() {
	// Cold cache
	s.Require().Equal(0, s.GetKeyCount())

	// Get service
	svc, err := s.cache.Get(s.Ctx, s.testService.Id)
	s.Require().NoError(err)
	s.Require().NotNil(svc)
	s.Require().Equal(s.testService.Id, svc.Id)

	// Verify stored in Redis
	redisKey := s.RedisClient.KB().CacheKey(serviceCacheType, s.testService.Id)
	s.RequireKeyExists(redisKey)
}

// TestServiceCache_GetFromL1 verifies L1 cache hit.
func (s *ServiceCacheTestSuite) TestServiceCache_GetFromL1() {
	// Pre-populate
	svc1, err := s.cache.Get(s.Ctx, s.testService.Id)
	s.Require().NoError(err)

	// Second get should hit L1
	svc2, err := s.cache.Get(s.Ctx, s.testService.Id)
	s.Require().NoError(err)
	s.Require().Equal(svc1, svc2)
}

// TestServiceCache_Invalidate verifies invalidation clears cache.
func (s *ServiceCacheTestSuite) TestServiceCache_Invalidate() {
	// Pre-populate
	_, err := s.cache.Get(s.Ctx, s.testService.Id)
	s.Require().NoError(err)

	// Invalidate
	err = s.cache.Invalidate(s.Ctx, s.testService.Id)
	s.Require().NoError(err)

	// Verify cleared from Redis
	redisKey := s.RedisClient.KB().CacheKey(serviceCacheType, s.testService.Id)
	s.RequireKeyNotExists(redisKey)
}

// TestServiceCache_Set verifies explicit Set works.
func (s *ServiceCacheTestSuite) TestServiceCache_Set() {
	// Set service
	err := s.cache.Set(s.Ctx, s.testService.Id, s.testService, 0)
	s.Require().NoError(err)

	// Verify in Redis
	redisKey := s.RedisClient.KB().CacheKey(serviceCacheType, s.testService.Id)
	s.RequireKeyExists(redisKey)

	// Get should return from L1
	svc, err := s.cache.Get(s.Ctx, s.testService.Id)
	s.Require().NoError(err)
	s.Require().Equal(s.testService.Id, svc.Id)
}
