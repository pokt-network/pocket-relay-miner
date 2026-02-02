//go:build test

package miner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hashicorp/go-version"
	"github.com/stretchr/testify/suite"

	localclient "github.com/pokt-network/pocket-relay-miner/client"
	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	pocktclient "github.com/pokt-network/poktroll/pkg/client"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// Test constants - deterministic for reproducibility
const (
	testSupplierAddr = "pokt1supplier1234567890abcdef"
	testAppAddr      = "pokt1application1234567890abc"
	testServiceID    = "anvil"
)

// LifecycleCallbackStatesSuite tests all state transitions in LifecycleCallback.
// Uses miniredis for real Redis operations (Rule #1: no mocks).
type LifecycleCallbackStatesSuite struct {
	suite.Suite

	// Miniredis instance shared across tests
	miniRedis   *miniredis.Miniredis
	redisClient *redisutil.Client
	ctx         context.Context

	// System under test
	callback *LifecycleCallback

	// Mocks (interfaces only, not full implementations)
	smstManager        *lcTestSMSTManager
	txClient           *lcTestTxClient
	sharedClient       *lcTestSharedQueryClient
	blockClient        *lcTestBlockClient
	sessionCoordinator *SessionCoordinator
	sessionStore       *RedisSessionStore
	deduplicator       *lcTestDeduplicator
}

// SetupSuite initializes the shared miniredis instance.
func (s *LifecycleCallbackStatesSuite) SetupSuite() {
	mr, err := miniredis.Run()
	s.Require().NoError(err, "failed to create miniredis")
	s.miniRedis = mr

	s.ctx = context.Background()

	redisURL := fmt.Sprintf("redis://%s", mr.Addr())
	client, err := redisutil.NewClient(s.ctx, redisutil.ClientConfig{URL: redisURL})
	s.Require().NoError(err, "failed to create Redis client")
	s.redisClient = client
}

// SetupTest runs before each test, creating fresh mocks and callback.
func (s *LifecycleCallbackStatesSuite) SetupTest() {
	s.miniRedis.FlushAll()

	// Create mocks
	s.smstManager = newLCTestSMSTManager()
	s.txClient = newLCTestTxClient()
	s.sharedClient = newLCTestSharedQueryClient()
	s.blockClient = newLCTestBlockClient(100) // Start at height 100

	// Create real session store with Redis
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	s.sessionStore = NewRedisSessionStore(logger, s.redisClient, SessionStoreConfig{
		KeyPrefix:       "test:sessions",
		SupplierAddress: testSupplierAddr,
	})

	// Create session coordinator
	s.sessionCoordinator = NewSessionCoordinator(logger, s.sessionStore, SMSTRecoveryConfig{
		SupplierAddress: testSupplierAddr,
	})

	// Create deduplicator
	s.deduplicator = newLCTestDeduplicator()

	// Create lifecycle callback
	s.callback = NewLifecycleCallback(
		logger,
		s.txClient,
		s.sharedClient,
		s.blockClient,
		nil, // sessionClient - not needed for state transition tests
		s.smstManager,
		s.sessionCoordinator,
		nil, // proofChecker - we'll set this per-test when needed
		LifecycleCallbackConfig{
			SupplierAddress:    testSupplierAddr,
			ClaimRetryAttempts: 1, // Single attempt for fast tests
			ClaimRetryDelay:    0,
			ProofRetryAttempts: 1,
			ProofRetryDelay:    0,
		},
	)
	s.callback.SetDeduplicator(s.deduplicator)
}

// TearDownSuite cleans up the shared miniredis.
func (s *LifecycleCallbackStatesSuite) TearDownSuite() {
	if s.miniRedis != nil {
		s.miniRedis.Close()
	}
	if s.redisClient != nil {
		s.redisClient.Close()
	}
}

// newTestSnapshot creates a deterministic SessionSnapshot for testing.
// seed provides unique but reproducible values.
// Uses fixed session heights to make window calculations predictable.
func (s *LifecycleCallbackStatesSuite) newTestSnapshot(seed int) *SessionSnapshot {
	// Use fixed heights: session ends at 100, so:
	// - Claim window opens at: 100 + 1 (grace) + 1 (claim_open) = 102
	// - Claim window closes at: 100 + 1 (grace) + 4 (claim_close) + 1 = 106
	// - Proof window opens at: 106 + 0 (proof_open) = 106
	// - Proof window closes at: 106 + 4 (proof_close) = 110
	return &SessionSnapshot{
		SessionID:               fmt.Sprintf("session-%08x", seed),
		SupplierOperatorAddress: testSupplierAddr,
		ServiceID:               testServiceID,
		ApplicationAddress:      testAppAddr,
		SessionStartHeight:      96,  // Fixed
		SessionEndHeight:        100, // Fixed - claim window 102-106, proof window 106-110
		State:                   SessionStateActive,
		RelayCount:              int64(seed*10 + 50),
		TotalComputeUnits:       uint64((seed*10 + 50) * 100),
		CreatedAt:               time.Now(),
		LastUpdatedAt:           time.Now(),
	}
}

// =============================================================================
// Claim Path Tests
// =============================================================================

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_Success() {
	// Setup: Create a session in Redis
	snapshot := s.newTestSnapshot(1)
	snapshot.RelayCount = 100
	snapshot.TotalComputeUnits = 10000

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at claim window open (102), with 4 blocks remaining until close (106)
	// Need at least 3 blocks remaining (minBlocksRequired=2 check is <=)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify
	s.Require().NoError(err)
	s.Require().Len(rootHashes, 1)
	s.Require().NotNil(rootHashes[0])
	s.Require().Equal(1, s.smstManager.getFlushCalls())
	s.Require().Equal(1, s.txClient.getClaimCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_BatchMultipleSessions() {
	// Setup: Create multiple sessions with same end height (all use default from newTestSnapshot)
	snapshots := make([]*SessionSnapshot, 3)
	for i := 0; i < 3; i++ {
		snapshots[i] = s.newTestSnapshot(10 + i)
		// All have same session end height (100) from newTestSnapshot
		snapshots[i].RelayCount = 100
		snapshots[i].TotalComputeUnits = 10000

		err := s.sessionStore.Save(s.ctx, snapshots[i])
		s.Require().NoError(err)
	}

	// Set block height at claim window open (102)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, snapshots)

	// Verify
	s.Require().NoError(err)
	s.Require().Len(rootHashes, 3)
	s.Require().Equal(3, s.smstManager.getFlushCalls()) // One flush per session
	s.Require().Equal(1, s.txClient.getClaimCalls())    // Single batched TX
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_FlushTreeError() {
	// Setup: Configure SMST to fail on flush
	s.smstManager.setFlushErr(errors.New("smst flush failed"))

	snapshot := s.newTestSnapshot(2)
	snapshot.RelayCount = 50
	snapshot.TotalComputeUnits = 5000

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at claim window open (102)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with SMST error
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "smst flush failed")
	s.Require().Nil(rootHashes)
	s.Require().Equal(0, s.txClient.getClaimCalls()) // TX should not be attempted
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_TxSubmissionError() {
	// Setup: Configure TX client to fail
	s.txClient.setCreateClaimsErr(errors.New("tx submission failed"))

	snapshot := s.newTestSnapshot(3)
	snapshot.RelayCount = 75
	snapshot.TotalComputeUnits = 7500

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at claim window open (102)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with TX error
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "tx submission failed")
	s.Require().Nil(rootHashes)
	s.Require().Equal(1, s.txClient.getClaimCalls()) // TX was attempted
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_WindowTimeout() {
	// Setup: Block height past claim window close
	snapshot := s.newTestSnapshot(4)
	// Session ends at 100, claim window closes at 106
	snapshot.RelayCount = 50
	snapshot.TotalComputeUnits = 5000

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height past claim window close (106) - only 0 blocks remaining
	s.blockClient.setHeight(106)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with window timeout
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "insufficient time")
	s.Require().Nil(rootHashes)
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_ZeroRelaysSkipped() {
	// Setup: Session with 0 relays
	snapshot := s.newTestSnapshot(5)
	snapshot.RelayCount = 0
	snapshot.TotalComputeUnits = 0

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at claim window open (102)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: No error, array allocated but session was skipped
	s.Require().NoError(err)
	s.Require().Len(rootHashes, 1) // Array allocated to input size
	s.Require().Nil(rootHashes[0]) // Session filtered - slot remains nil

	// NOTE: Characterization - CreateClaims is called even with 0 valid sessions
	// This is the actual current behavior (no early return when all sessions filtered)
	s.Require().Equal(1, s.txClient.getClaimCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_AlreadyClaimedSkipped() {
	// Setup: Session with existing claim TX hash (already submitted)
	snapshot := s.newTestSnapshot(6)
	snapshot.RelayCount = 100
	snapshot.TotalComputeUnits = 10000
	snapshot.ClaimTxHash = "ABC123DEF456"

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at claim window open (102)
	s.blockClient.setHeight(102)

	// Execute
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: No error, array allocated but session was skipped (deduplication)
	s.Require().NoError(err)
	s.Require().Len(rootHashes, 1) // Array allocated to input size
	s.Require().Nil(rootHashes[0]) // Session filtered - slot remains nil

	// NOTE: Characterization - CreateClaims is called even with 0 valid sessions
	// This is the actual current behavior (no early return when all sessions filtered)
	s.Require().Equal(1, s.txClient.getClaimCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedClaim_EmptyInput() {
	// Execute with empty snapshot list
	rootHashes, err := s.callback.OnSessionsNeedClaim(s.ctx, []*SessionSnapshot{})

	// Verify: No error, empty result
	s.Require().NoError(err)
	s.Require().Nil(rootHashes)
}

// =============================================================================
// Proof Path Tests
// =============================================================================

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_Success() {
	// Setup: Create a claimed session
	snapshot := s.newTestSnapshot(10)
	snapshot.State = SessionStateClaimed
	snapshot.RelayCount = 100
	snapshot.TotalComputeUnits = 10000
	snapshot.ClaimedRootHash = []byte("claimed-root-hash")

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at proof window open (106), with 4 blocks remaining until close (110)
	s.blockClient.setHeight(106)

	// Execute
	err = s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{snapshot})

	// Verify
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getProveCalls())
	s.Require().Equal(1, s.txClient.getProofCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_BatchMultipleSessions() {
	// Setup: Create multiple claimed sessions with same end height (all use default from newTestSnapshot)
	snapshots := make([]*SessionSnapshot, 3)
	for i := 0; i < 3; i++ {
		snapshots[i] = s.newTestSnapshot(20 + i)
		snapshots[i].State = SessionStateClaimed
		// All have same session end height (100) from newTestSnapshot
		snapshots[i].RelayCount = 100
		snapshots[i].TotalComputeUnits = 10000
		snapshots[i].ClaimedRootHash = []byte(fmt.Sprintf("claimed-root-%d", i))

		err := s.sessionStore.Save(s.ctx, snapshots[i])
		s.Require().NoError(err)
	}

	// Set block height at proof window open (106)
	s.blockClient.setHeight(106)

	// Execute
	err := s.callback.OnSessionsNeedProof(s.ctx, snapshots)

	// Verify
	s.Require().NoError(err)
	s.Require().Equal(3, s.smstManager.getProveCalls()) // One proof per session
	s.Require().Equal(1, s.txClient.getProofCalls())    // Single batched TX
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_ProveClosestError() {
	// Setup: Configure SMST to fail on proof generation
	s.smstManager.setProveErr(errors.New("proof generation failed"))

	snapshot := s.newTestSnapshot(31)
	snapshot.State = SessionStateClaimed
	snapshot.RelayCount = 50
	snapshot.TotalComputeUnits = 5000
	snapshot.ClaimedRootHash = []byte("claimed-root")

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at proof window open (106)
	s.blockClient.setHeight(106)

	// Execute
	err = s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with proof generation error
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "proof generation failed")
	s.Require().Equal(0, s.txClient.getProofCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_TxSubmissionError() {
	// Setup: Configure TX client to fail
	s.txClient.setSubmitProofsErr(errors.New("proof tx failed"))

	snapshot := s.newTestSnapshot(32)
	snapshot.State = SessionStateClaimed
	snapshot.RelayCount = 75
	snapshot.TotalComputeUnits = 7500
	snapshot.ClaimedRootHash = []byte("claimed-root")

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height at proof window open (106)
	s.blockClient.setHeight(106)

	// Execute
	err = s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with TX error
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "proof tx failed")
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_WindowTimeout() {
	// Setup: Block height past proof window close
	snapshot := s.newTestSnapshot(33)
	snapshot.State = SessionStateClaimed
	snapshot.SessionStartHeight = 100
	snapshot.SessionEndHeight = 104
	snapshot.RelayCount = 50
	snapshot.TotalComputeUnits = 5000
	snapshot.ClaimedRootHash = []byte("claimed-root")

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Set block height way past proof window
	s.blockClient.setHeight(150)

	// Execute
	err = s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: Should fail with window timeout
	s.Require().Error(err)
	s.Require().Contains(err.Error(), "proof window")
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_AlreadyProvedSkipped() {
	// Setup: Session with existing proof TX hash
	snapshot := s.newTestSnapshot(34)
	snapshot.State = SessionStateClaimed
	snapshot.RelayCount = 100
	snapshot.TotalComputeUnits = 10000
	snapshot.ClaimedRootHash = []byte("claimed-root")
	snapshot.ProofTxHash = "PROOF123ABC"

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	s.blockClient.setHeight(snapshot.SessionEndHeight + 10)

	// Execute
	err = s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{snapshot})

	// Verify: No error, no proof submitted (deduplication)
	s.Require().NoError(err)
	s.Require().Equal(0, s.txClient.getProofCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionsNeedProof_EmptyInput() {
	// Execute with empty snapshot list
	err := s.callback.OnSessionsNeedProof(s.ctx, []*SessionSnapshot{})

	// Verify: No error
	s.Require().NoError(err)
}

// =============================================================================
// Terminal State Callbacks
// =============================================================================

func (s *LifecycleCallbackStatesSuite) TestOnSessionProved_CleansResources() {
	snapshot := s.newTestSnapshot(40)
	snapshot.State = SessionStateProved
	snapshot.RelayCount = 100

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Execute
	err = s.callback.OnSessionProved(s.ctx, snapshot)

	// Verify: Resources cleaned up
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
	s.Require().Contains(s.deduplicator.getCleanedSessions(), snapshot.SessionID)
}

func (s *LifecycleCallbackStatesSuite) TestOnProbabilisticProved_CleansResources() {
	snapshot := s.newTestSnapshot(41)
	snapshot.State = SessionStateProbabilisticProved
	snapshot.RelayCount = 100

	err := s.sessionStore.Save(s.ctx, snapshot)
	s.Require().NoError(err)

	// Execute
	err = s.callback.OnProbabilisticProved(s.ctx, snapshot)

	// Verify: Resources cleaned up (same as proved path)
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
	s.Require().Contains(s.deduplicator.getCleanedSessions(), snapshot.SessionID)
}

func (s *LifecycleCallbackStatesSuite) TestOnClaimWindowClosed_CleansResources() {
	snapshot := s.newTestSnapshot(42)
	snapshot.State = SessionStateClaimWindowClosed
	snapshot.RelayCount = 100

	// Execute
	err := s.callback.OnClaimWindowClosed(s.ctx, snapshot)

	// Verify: SMST deleted on failure
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnClaimTxError_CleansResources() {
	snapshot := s.newTestSnapshot(43)
	snapshot.State = SessionStateClaimTxError
	snapshot.RelayCount = 100

	// Execute
	err := s.callback.OnClaimTxError(s.ctx, snapshot)

	// Verify: SMST deleted on failure
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnProofWindowClosed_CleansResources() {
	snapshot := s.newTestSnapshot(44)
	snapshot.State = SessionStateProofWindowClosed
	snapshot.RelayCount = 100

	// Execute
	err := s.callback.OnProofWindowClosed(s.ctx, snapshot)

	// Verify: SMST deleted on failure
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnProofTxError_CleansResources() {
	snapshot := s.newTestSnapshot(45)
	snapshot.State = SessionStateProofTxError
	snapshot.RelayCount = 100

	// Execute
	err := s.callback.OnProofTxError(s.ctx, snapshot)

	// Verify: SMST deleted on failure
	s.Require().NoError(err)
	s.Require().Equal(1, s.smstManager.getDeleteCalls())
}

func (s *LifecycleCallbackStatesSuite) TestOnSessionActive_Informational() {
	snapshot := s.newTestSnapshot(50)
	snapshot.State = SessionStateActive

	// Execute - should just log, not error
	err := s.callback.OnSessionActive(s.ctx, snapshot)

	// Verify: No error, no resource operations
	s.Require().NoError(err)
	s.Require().Equal(0, s.smstManager.getDeleteCalls())
}

// =============================================================================
// Test Helpers / Mocks - prefixed with lc to avoid naming conflicts
// =============================================================================

// lcTestSMSTManager implements SMSTManager for testing.
type lcTestSMSTManager struct {
	mu          sync.Mutex
	flushCalls  int
	proveCalls  int
	deleteCalls int
	flushErr    error
	proveErr    error
	deleteErr   error
}

func newLCTestSMSTManager() *lcTestSMSTManager {
	return &lcTestSMSTManager{}
}

func (m *lcTestSMSTManager) FlushTree(ctx context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushCalls++
	if m.flushErr != nil {
		return nil, m.flushErr
	}
	return []byte("mock-root-hash-" + sessionID), nil
}

func (m *lcTestSMSTManager) GetTreeRoot(ctx context.Context, sessionID string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return []byte("mock-root-hash-" + sessionID), nil
}

func (m *lcTestSMSTManager) ProveClosest(ctx context.Context, sessionID string, path []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proveCalls++
	if m.proveErr != nil {
		return nil, m.proveErr
	}
	return []byte("mock-proof-" + sessionID), nil
}

func (m *lcTestSMSTManager) DeleteTree(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	return m.deleteErr
}

func (m *lcTestSMSTManager) setFlushErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushErr = err
}

func (m *lcTestSMSTManager) setProveErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.proveErr = err
}

func (m *lcTestSMSTManager) getFlushCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushCalls
}

func (m *lcTestSMSTManager) getProveCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proveCalls
}

func (m *lcTestSMSTManager) getDeleteCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteCalls
}

// lcTestTxClient implements pocktclient.SupplierClient for testing.
type lcTestTxClient struct {
	mu              sync.Mutex
	claimCalls      int
	proofCalls      int
	createClaimsErr error
	submitProofsErr error
}

func newLCTestTxClient() *lcTestTxClient {
	return &lcTestTxClient{}
}

func (c *lcTestTxClient) CreateClaims(ctx context.Context, timeoutHeight int64, claims ...pocktclient.MsgCreateClaim) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.claimCalls++
	return c.createClaimsErr
}

func (c *lcTestTxClient) SubmitProofs(ctx context.Context, timeoutHeight int64, proofs ...pocktclient.MsgSubmitProof) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.proofCalls++
	return c.submitProofsErr
}

func (c *lcTestTxClient) OperatorAddress() string {
	return testSupplierAddr
}

func (c *lcTestTxClient) setCreateClaimsErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.createClaimsErr = err
}

func (c *lcTestTxClient) setSubmitProofsErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submitProofsErr = err
}

func (c *lcTestTxClient) getClaimCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.claimCalls
}

func (c *lcTestTxClient) getProofCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.proofCalls
}

// lcTestSharedQueryClient implements pocktclient.SharedQueryClient for testing.
type lcTestSharedQueryClient struct {
	params *sharedtypes.Params
}

func newLCTestSharedQueryClient() *lcTestSharedQueryClient {
	return &lcTestSharedQueryClient{
		params: &sharedtypes.Params{
			NumBlocksPerSession:            4,
			GracePeriodEndOffsetBlocks:     1,
			ClaimWindowOpenOffsetBlocks:    1,
			ClaimWindowCloseOffsetBlocks:   4,
			ProofWindowOpenOffsetBlocks:    0,
			ProofWindowCloseOffsetBlocks:   4,
			ComputeUnitsToTokensMultiplier: 42,
		},
	}
}

func (c *lcTestSharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	return c.params, nil
}

func (c *lcTestSharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return sharedtypes.GetSessionGracePeriodEndHeight(c.params, queryHeight), nil
}

func (c *lcTestSharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return sharedtypes.GetClaimWindowOpenHeight(c.params, queryHeight), nil
}

func (c *lcTestSharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierAddr string) (int64, error) {
	return queryHeight + 1, nil
}

func (c *lcTestSharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	return sharedtypes.GetProofWindowOpenHeight(c.params, queryHeight), nil
}

func (c *lcTestSharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierAddr string) (int64, error) {
	return queryHeight + 1, nil
}

// lcTestBlockClient implements pocktclient.BlockClient for testing.
type lcTestBlockClient struct {
	mu            sync.RWMutex
	currentHeight int64
	blockHash     []byte
	subscribers   []chan *localclient.SimpleBlock
}

func newLCTestBlockClient(height int64) *lcTestBlockClient {
	return &lcTestBlockClient{
		currentHeight: height,
		blockHash:     []byte("test-block-hash"),
	}
}

func (c *lcTestBlockClient) LastBlock(ctx context.Context) pocktclient.Block {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &lcTestBlock{
		height: c.currentHeight,
		hash:   c.blockHash,
	}
}

func (c *lcTestBlockClient) CommittedBlocksSequence(ctx context.Context) pocktclient.BlockReplayObservable {
	return nil
}

func (c *lcTestBlockClient) Close() {}

func (c *lcTestBlockClient) GetChainVersion() *version.Version {
	v, _ := version.NewVersion("0.1.0")
	return v
}

func (c *lcTestBlockClient) Subscribe(ctx context.Context, bufferSize int) <-chan *localclient.SimpleBlock {
	c.mu.Lock()
	defer c.mu.Unlock()

	ch := make(chan *localclient.SimpleBlock, bufferSize)
	c.subscribers = append(c.subscribers, ch)

	// Immediately send current block
	go func() {
		c.mu.RLock()
		block := localclient.NewSimpleBlock(c.currentHeight, c.blockHash, time.Now())
		c.mu.RUnlock()
		ch <- block
	}()

	return ch
}

func (c *lcTestBlockClient) GetBlockAtHeight(ctx context.Context, height int64) (pocktclient.Block, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &lcTestBlock{
		height: height,
		hash:   c.blockHash,
	}, nil
}

func (c *lcTestBlockClient) setHeight(height int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.currentHeight = height

	// Notify subscribers
	for _, ch := range c.subscribers {
		select {
		case ch <- localclient.NewSimpleBlock(c.currentHeight, c.blockHash, time.Now()):
		default:
		}
	}
}

type lcTestBlock struct {
	height int64
	hash   []byte
}

func (b *lcTestBlock) Height() int64 {
	return b.height
}

func (b *lcTestBlock) Hash() []byte {
	if b.hash == nil {
		return []byte("mock-block-hash")
	}
	return b.hash
}

// lcTestDeduplicator implements Deduplicator for testing.
type lcTestDeduplicator struct {
	mu              sync.Mutex
	cleanedSessions []string
	cleanupErr      error
}

func newLCTestDeduplicator() *lcTestDeduplicator {
	return &lcTestDeduplicator{}
}

func (d *lcTestDeduplicator) IsDuplicate(ctx context.Context, hash []byte, sessionID string) (bool, error) {
	return false, nil
}

func (d *lcTestDeduplicator) MarkProcessed(ctx context.Context, hash []byte, sessionID string) error {
	return nil
}

func (d *lcTestDeduplicator) MarkProcessedBatch(ctx context.Context, hashes [][]byte, sessionID string) error {
	return nil
}

func (d *lcTestDeduplicator) CleanupSession(ctx context.Context, sessionID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cleanedSessions = append(d.cleanedSessions, sessionID)
	return d.cleanupErr
}

func (d *lcTestDeduplicator) Start(ctx context.Context) error {
	return nil
}

func (d *lcTestDeduplicator) Close() error {
	return nil
}

func (d *lcTestDeduplicator) getCleanedSessions() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string{}, d.cleanedSessions...)
}

// lcTestSessionQueryClient implements SessionQueryClient for testing.
type lcTestSessionQueryClient struct {
	mu       sync.Mutex
	sessions map[string]*sessiontypes.Session
}

func newLCTestSessionQueryClient() *lcTestSessionQueryClient {
	return &lcTestSessionQueryClient{
		sessions: make(map[string]*sessiontypes.Session),
	}
}

func (c *lcTestSessionQueryClient) GetSession(ctx context.Context, appAddr, serviceID string, blockHeight int64) (*sessiontypes.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return a mock session with minimal fields
	return &sessiontypes.Session{
		Header: &sessiontypes.SessionHeader{
			ApplicationAddress:      appAddr,
			ServiceId:               serviceID,
			SessionStartBlockHeight: blockHeight,
			SessionEndBlockHeight:   blockHeight + 4,
		},
	}, nil
}

// =============================================================================
// Test Runner
// =============================================================================

func TestLifecycleCallbackStatesSuite(t *testing.T) {
	suite.Run(t, new(LifecycleCallbackStatesSuite))
}
