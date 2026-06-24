package miner

import (
	"context"
	"sync"

	"github.com/hashicorp/go-version"
	"github.com/pokt-network/poktroll/pkg/client"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// Compile-time interface assertions so the unused linter counts these types as used.
var (
	_ client.SharedQueryClient = (*mockSharedQueryClient)(nil)
	_ client.BlockClient       = (*mockBlockClient)(nil)
	_ client.Block             = (*mockBlock)(nil)
)

// mockSharedQueryClient implements client.SharedQueryClient for testing.
type mockSharedQueryClient struct {
	params *sharedtypes.Params

	// paramsAtHeightFn, when set, overrides GetParamsAtHeight so tests can return
	// height-specific params (simulating params that were effective at an older
	// session height). When nil, GetParamsAtHeight delegates to GetParams.
	paramsAtHeightFn func(ctx context.Context, queryHeight int64) (*sharedtypes.Params, error)
}

func (m *mockSharedQueryClient) GetParams(ctx context.Context) (*sharedtypes.Params, error) {
	if m.params == nil {
		return &sharedtypes.Params{
			NumBlocksPerSession:            4,
			GracePeriodEndOffsetBlocks:     1,
			ClaimWindowOpenOffsetBlocks:    1,
			ClaimWindowCloseOffsetBlocks:   4,
			ProofWindowOpenOffsetBlocks:    0,
			ProofWindowCloseOffsetBlocks:   4,
			ComputeUnitsToTokensMultiplier: 42,
		}, nil
	}
	return m.params, nil
}

func (m *mockSharedQueryClient) GetParamsAtHeight(ctx context.Context, queryHeight int64) (*sharedtypes.Params, error) {
	if m.paramsAtHeightFn != nil {
		return m.paramsAtHeightFn(ctx, queryHeight)
	}
	return m.GetParams(ctx)
}

func (m *mockSharedQueryClient) GetSessionGracePeriodEndHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetSessionGracePeriodEndHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetClaimWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetClaimWindowOpenHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetEarliestSupplierClaimCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return queryHeight + 1, nil
}

func (m *mockSharedQueryClient) GetProofWindowOpenHeight(ctx context.Context, queryHeight int64) (int64, error) {
	params, _ := m.GetParams(ctx)
	return sharedtypes.GetProofWindowOpenHeight(params, queryHeight), nil
}

func (m *mockSharedQueryClient) GetEarliestSupplierProofCommitHeight(ctx context.Context, queryHeight int64, supplierOperatorAddr string) (int64, error) {
	return queryHeight + 1, nil
}

// mockBlockClient implements client.BlockClient for testing.
type mockBlockClient struct {
	mu            sync.RWMutex
	currentHeight int64
	blockHash     []byte
}

func (m *mockBlockClient) LastBlock(ctx context.Context) client.Block {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return &mockBlock{
		height: m.currentHeight,
		hash:   m.blockHash,
	}
}

func (m *mockBlockClient) CommittedBlocksSequence(ctx context.Context) client.BlockReplayObservable {
	return nil
}

func (m *mockBlockClient) Close() {}

func (m *mockBlockClient) GetChainVersion() *version.Version {
	v, _ := version.NewVersion("0.1.0")
	return v
}

// mockBlock implements client.Block for testing.
type mockBlock struct {
	height int64
	hash   []byte
}

func (b *mockBlock) Height() int64 {
	return b.height
}

func (b *mockBlock) Hash() []byte {
	if b.hash == nil {
		return []byte("mock-block-hash")
	}
	return b.hash
}
