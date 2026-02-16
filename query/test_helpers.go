//go:build test

package query

import (
	"context"
	"fmt"
	"net"
	"testing"

	cosmostypes "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apptypes "github.com/pokt-network/poktroll/x/application/types"
	prooftypes "github.com/pokt-network/poktroll/x/proof/types"
	servicetypes "github.com/pokt-network/poktroll/x/service/types"
	sessiontypes "github.com/pokt-network/poktroll/x/session/types"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
	suppliertypes "github.com/pokt-network/poktroll/x/supplier/types"
)

// mockQueryServer provides a mock gRPC server for testing query clients
type mockQueryServer struct {
	// Session responses
	getSessionFunc func(context.Context, *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error)
	sessionParams  *sessiontypes.Params

	// Application responses
	getApplicationFunc func(context.Context, *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error)
	applicationParams  *apptypes.Params

	// Shared responses
	sharedParams *sharedtypes.Params

	// Supplier responses
	getSupplierFunc func(context.Context, *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error)
	supplierParams  *suppliertypes.Params

	// Proof responses
	getClaimFunc func(context.Context, *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error)
	proofParams  *prooftypes.Params

	// Service responses
	getServiceFunc                       func(context.Context, *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error)
	getRelayMiningDifficultyFunc         func(context.Context, *servicetypes.QueryGetRelayMiningDifficultyRequest) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error)
	getRelayMiningDifficultyAtHeightFunc func(context.Context, *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error)
	serviceParams                        *servicetypes.Params
}

// Session query server implementation
type mockSessionQueryServer struct {
	sessiontypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockSessionQueryServer) GetSession(ctx context.Context, req *sessiontypes.QueryGetSessionRequest) (*sessiontypes.QueryGetSessionResponse, error) {
	if m.mock.getSessionFunc != nil {
		return m.mock.getSessionFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "session not found")
}

func (m *mockSessionQueryServer) Params(ctx context.Context, req *sessiontypes.QueryParamsRequest) (*sessiontypes.QueryParamsResponse, error) {
	if m.mock.sessionParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &sessiontypes.QueryParamsResponse{Params: *m.mock.sessionParams}, nil
}

// Application query server implementation
type mockApplicationQueryServer struct {
	apptypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockApplicationQueryServer) Application(ctx context.Context, req *apptypes.QueryGetApplicationRequest) (*apptypes.QueryGetApplicationResponse, error) {
	if m.mock.getApplicationFunc != nil {
		return m.mock.getApplicationFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "application not found")
}

func (m *mockApplicationQueryServer) AllApplications(ctx context.Context, req *apptypes.QueryAllApplicationsRequest) (*apptypes.QueryAllApplicationsResponse, error) {
	return &apptypes.QueryAllApplicationsResponse{Applications: []apptypes.Application{}}, nil
}

func (m *mockApplicationQueryServer) Params(ctx context.Context, req *apptypes.QueryParamsRequest) (*apptypes.QueryParamsResponse, error) {
	if m.mock.applicationParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &apptypes.QueryParamsResponse{Params: *m.mock.applicationParams}, nil
}

// Shared query server implementation
type mockSharedQueryServer struct {
	sharedtypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockSharedQueryServer) Params(ctx context.Context, req *sharedtypes.QueryParamsRequest) (*sharedtypes.QueryParamsResponse, error) {
	if m.mock.sharedParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &sharedtypes.QueryParamsResponse{Params: *m.mock.sharedParams}, nil
}

// Supplier query server implementation
type mockSupplierQueryServer struct {
	suppliertypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockSupplierQueryServer) Supplier(ctx context.Context, req *suppliertypes.QueryGetSupplierRequest) (*suppliertypes.QueryGetSupplierResponse, error) {
	if m.mock.getSupplierFunc != nil {
		return m.mock.getSupplierFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "supplier not found")
}

func (m *mockSupplierQueryServer) Params(ctx context.Context, req *suppliertypes.QueryParamsRequest) (*suppliertypes.QueryParamsResponse, error) {
	if m.mock.supplierParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &suppliertypes.QueryParamsResponse{Params: *m.mock.supplierParams}, nil
}

// Proof query server implementation
type mockProofQueryServer struct {
	prooftypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockProofQueryServer) Claim(ctx context.Context, req *prooftypes.QueryGetClaimRequest) (*prooftypes.QueryGetClaimResponse, error) {
	if m.mock.getClaimFunc != nil {
		return m.mock.getClaimFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "claim not found")
}

func (m *mockProofQueryServer) Params(ctx context.Context, req *prooftypes.QueryParamsRequest) (*prooftypes.QueryParamsResponse, error) {
	if m.mock.proofParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &prooftypes.QueryParamsResponse{Params: *m.mock.proofParams}, nil
}

// Service query server implementation
type mockServiceQueryServer struct {
	servicetypes.UnimplementedQueryServer
	mock *mockQueryServer
}

func (m *mockServiceQueryServer) Service(ctx context.Context, req *servicetypes.QueryGetServiceRequest) (*servicetypes.QueryGetServiceResponse, error) {
	if m.mock.getServiceFunc != nil {
		return m.mock.getServiceFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "service not found")
}

func (m *mockServiceQueryServer) RelayMiningDifficulty(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyRequest) (*servicetypes.QueryGetRelayMiningDifficultyResponse, error) {
	if m.mock.getRelayMiningDifficultyFunc != nil {
		return m.mock.getRelayMiningDifficultyFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "difficulty not found")
}

func (m *mockServiceQueryServer) RelayMiningDifficultyAtHeight(ctx context.Context, req *servicetypes.QueryGetRelayMiningDifficultyAtHeightRequest) (*servicetypes.QueryGetRelayMiningDifficultyAtHeightResponse, error) {
	if m.mock.getRelayMiningDifficultyAtHeightFunc != nil {
		return m.mock.getRelayMiningDifficultyAtHeightFunc(ctx, req)
	}
	return nil, status.Error(codes.NotFound, "difficulty at height not found")
}

func (m *mockServiceQueryServer) Params(ctx context.Context, req *servicetypes.QueryParamsRequest) (*servicetypes.QueryParamsResponse, error) {
	if m.mock.serviceParams == nil {
		return nil, status.Error(codes.NotFound, "params not found")
	}
	return &servicetypes.QueryParamsResponse{Params: *m.mock.serviceParams}, nil
}

// setupMockQueryServer creates a mock gRPC server for testing
func setupMockQueryServer(t *testing.T) (server *grpc.Server, address string, cleanup func(), mock *mockQueryServer) {
	t.Helper()

	// Create listener
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	// Create gRPC server
	server = grpc.NewServer()
	mock = &mockQueryServer{}

	// Register all services with their respective mock servers
	sessiontypes.RegisterQueryServer(server, &mockSessionQueryServer{mock: mock})
	apptypes.RegisterQueryServer(server, &mockApplicationQueryServer{mock: mock})
	sharedtypes.RegisterQueryServer(server, &mockSharedQueryServer{mock: mock})
	suppliertypes.RegisterQueryServer(server, &mockSupplierQueryServer{mock: mock})
	prooftypes.RegisterQueryServer(server, &mockProofQueryServer{mock: mock})
	servicetypes.RegisterQueryServer(server, &mockServiceQueryServer{mock: mock})

	// Start server
	go func() {
		_ = server.Serve(listener)
	}()

	cleanup = func() {
		server.Stop()
		listener.Close()
	}

	return server, listener.Addr().String(), cleanup, mock
}

// generateTestSession creates a test session
func generateTestSession(appAddress string, serviceID string, sessionStartHeight int64) *sessiontypes.Session {
	return &sessiontypes.Session{
		Header: &sessiontypes.SessionHeader{
			ApplicationAddress:      appAddress,
			ServiceId:               serviceID,
			SessionId:               fmt.Sprintf("session_%s_%s_%d", appAddress, serviceID, sessionStartHeight),
			SessionStartBlockHeight: sessionStartHeight,
			SessionEndBlockHeight:   sessionStartHeight + 4,
		},
		SessionId:           fmt.Sprintf("session_%s_%s_%d", appAddress, serviceID, sessionStartHeight),
		SessionNumber:       1,
		NumBlocksPerSession: 4,
		Suppliers: []*sharedtypes.Supplier{
			{
				OperatorAddress: "pokt1supplier1",
				Services: []*sharedtypes.SupplierServiceConfig{
					{ServiceId: serviceID},
				},
			},
		},
		Application: &apptypes.Application{
			Address: appAddress,
		},
	}
}

// generateTestApplication creates a test application
func generateTestApplication(address string) *apptypes.Application {
	stake := cosmostypes.NewInt64Coin("upokt", 1000000)
	return &apptypes.Application{
		Address: address,
		Stake:   &stake,
		ServiceConfigs: []*sharedtypes.ApplicationServiceConfig{
			{ServiceId: "develop"},
		},
	}
}

// generateTestSharedParams creates test shared params
func generateTestSharedParams() *sharedtypes.Params {
	return &sharedtypes.Params{
		NumBlocksPerSession:                4,
		GracePeriodEndOffsetBlocks:         1,
		ClaimWindowOpenOffsetBlocks:        1,
		ClaimWindowCloseOffsetBlocks:       4,
		ProofWindowOpenOffsetBlocks:        0,
		ProofWindowCloseOffsetBlocks:       4,
		SupplierUnbondingPeriodSessions:    4,
		ApplicationUnbondingPeriodSessions: 4,
		ComputeUnitsToTokensMultiplier:     42,
	}
}

// generateTestSessionParams creates test session params
func generateTestSessionParams() *sessiontypes.Params {
	return &sessiontypes.Params{}
}

// generateTestProofParams creates test proof params
func generateTestProofParams() *prooftypes.Params {
	proofRequestProbability := float64(0.25)
	proofRequirementThreshold := cosmostypes.NewInt64Coin("upokt", 20)
	proofMissingPenalty := cosmostypes.NewInt64Coin("upokt", 320000000)

	return &prooftypes.Params{
		ProofRequestProbability:   proofRequestProbability,
		ProofRequirementThreshold: &proofRequirementThreshold,
		ProofMissingPenalty:       &proofMissingPenalty,
	}
}

// generateTestApplicationParams creates test application params
func generateTestApplicationParams() *apptypes.Params {
	minStake := cosmostypes.NewInt64Coin("upokt", 1000000)
	return &apptypes.Params{
		MinStake: &minStake,
	}
}

// generateTestSupplier creates a test supplier
func generateTestSupplier(operatorAddress string) *sharedtypes.Supplier {
	stake := cosmostypes.NewInt64Coin("upokt", 1000000)
	return &sharedtypes.Supplier{
		OperatorAddress: operatorAddress,
		OwnerAddress:    operatorAddress,
		Stake:           &stake,
		Services: []*sharedtypes.SupplierServiceConfig{
			{ServiceId: "develop"},
		},
	}
}

// generateTestSupplierParams creates test supplier params
func generateTestSupplierParams() *suppliertypes.Params {
	minStake := cosmostypes.NewInt64Coin("upokt", 1000000)
	return &suppliertypes.Params{
		MinStake: &minStake,
	}
}

// generateTestClaim creates a test claim
func generateTestClaim(supplierOperatorAddress string, sessionID string) *prooftypes.Claim {
	return &prooftypes.Claim{
		SupplierOperatorAddress: supplierOperatorAddress,
		SessionHeader: &sessiontypes.SessionHeader{
			SessionId: sessionID,
		},
		RootHash: []byte("test_root_hash"),
	}
}

// generateTestService creates a test service
func generateTestService(serviceID string) *sharedtypes.Service {
	return &sharedtypes.Service{
		Id:                   serviceID,
		Name:                 serviceID,
		ComputeUnitsPerRelay: 1,
		OwnerAddress:         "pokt1owner",
	}
}

// generateTestServiceParams creates test service params
func generateTestServiceParams() *servicetypes.Params {
	addServiceFee := cosmostypes.NewInt64Coin("upokt", 1000000)
	return &servicetypes.Params{
		AddServiceFee: &addServiceFee,
	}
}

// generateTestRelayMiningDifficulty creates test relay mining difficulty
func generateTestRelayMiningDifficulty(serviceID string) *servicetypes.RelayMiningDifficulty {
	return &servicetypes.RelayMiningDifficulty{
		ServiceId:    serviceID,
		BlockHeight:  100,
		NumRelaysEma: 1000,
		TargetHash:   make([]byte, 32),
	}
}
