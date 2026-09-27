package miner

import (
	"fmt"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// validateCacheTTLAgainstChainParams refuses to start the miner when its
// configured cache_ttl is shorter than a session plus its claim window can
// possibly need, given the chain's REAL params -- not a static default.
//
// Formula: (num_blocks_per_session + claim_window_open_offset_blocks + 1 +
// claim_window_close_offset_blocks) x block_time_seconds. This covers a
// session's cached data through the close of its claim window; the proof
// window that follows is covered separately because cache_ttl slides forward
// on every write (session_store.go's Expire, redis_mapstore.go's SMST TTL
// refresh), and the last write lands near the claim window's OPEN, restarting
// the full TTL well before the proof window's close.
func validateCacheTTLAgainstChainParams(cacheTTL time.Duration, blockTimeSeconds int64, params *sharedtypes.Params) error {
	required := time.Duration(
		int64(params.GetNumBlocksPerSession())+
			int64(params.GetClaimWindowOpenOffsetBlocks())+1+
			int64(params.GetClaimWindowCloseOffsetBlocks()),
	) * time.Duration(blockTimeSeconds) * time.Second
	if cacheTTL < required {
		return fmt.Errorf(
			"redis.cache_ttl (%s) is shorter than this chain's session+claim-window "+
				"coverage (%s, from num_blocks_per_session=%d + claim_window_open_offset_blocks=%d "+
				"+ 1 + claim_window_close_offset_blocks=%d at block_time_seconds=%d): "+
				"a session's cached data could expire before its claim window closes",
			cacheTTL, required, params.GetNumBlocksPerSession(),
			params.GetClaimWindowOpenOffsetBlocks(), params.GetClaimWindowCloseOffsetBlocks(),
			blockTimeSeconds)
	}
	return nil
}

// checkCacheTTL validates cache_ttl against shared params the caller already
// read, Warning (not erroring) when sharedErr says the chain could not be
// reached -- an unreachable node must not fail startup on its own, the same
// choice the shared params advisory makes for the same round trip.
func checkCacheTTL(cfg *Config, logger logging.Logger, sharedParams *sharedtypes.Params, sharedErr error) error {
	if sharedErr != nil {
		logger.Warn().Err(sharedErr).
			Msg("could not read shared params to validate cache_ttl; skipping the check")
		return nil
	}
	return validateCacheTTLAgainstChainParams(cfg.GetCacheTTL(), cfg.GetBlockTimeSeconds(), sharedParams)
}
