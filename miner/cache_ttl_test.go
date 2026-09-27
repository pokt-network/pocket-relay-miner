//go:build test

package miner

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

func TestValidateCacheTTLAgainstChainParams_TooShortIsAnError(t *testing.T) {
	params := &sharedtypes.Params{
		NumBlocksPerSession:          20,
		ClaimWindowOpenOffsetBlocks:  11,
		ClaimWindowCloseOffsetBlocks: 10,
	}
	// required = (20+11+1+10) * 60s = 42 * 60s = 2520s = 42m
	err := validateCacheTTLAgainstChainParams(41*time.Minute, 60, params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cache_ttl")

	err = validateCacheTTLAgainstChainParams(42*time.Minute, 60, params)
	require.NoError(t, err, "exactly the minimum must be accepted")
}

func TestCheckCacheTTL_ShortParamsIsAnError(t *testing.T) {
	cfg := &Config{CacheTTL: 41 * time.Minute, BlockTimeSeconds: 60}
	params := &sharedtypes.Params{
		NumBlocksPerSession:          20,
		ClaimWindowOpenOffsetBlocks:  11,
		ClaimWindowCloseOffsetBlocks: 10,
	}
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	err := checkCacheTTL(cfg, logger, params, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cache_ttl")
}

func TestCheckCacheTTL_UnreachableChainWarnsAndStarts(t *testing.T) {
	cfg := &Config{CacheTTL: 1 * time.Second, BlockTimeSeconds: 60} // absurdly short: would fail if validated
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())

	err := checkCacheTTL(cfg, logger, nil, errors.New("chain unreachable"))
	require.NoError(t, err, "an unreachable chain must not fail startup, even with an insufficient cache_ttl")
}
