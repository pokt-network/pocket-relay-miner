package miner

import (
	"context"
	"errors"
	"net"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// SMST errors - permanent, should not retry
var (
	// ErrSessionSealing indicates the session is being sealed for claim submission.
	ErrSessionSealing = errors.New("session is sealing for claim")

	// ErrSessionClaimed indicates the session has already been claimed.
	ErrSessionClaimed = errors.New("session has already been claimed")

	// ErrSMSTUpdateFailed indicates the SMST trie update failed (data/logic error).
	ErrSMSTUpdateFailed = errors.New("SMST trie update failed")

	// ErrSMSTCommitFailed indicates the SMST commit to store failed.
	ErrSMSTCommitFailed = errors.New("SMST commit failed")
)

// Supplier errors - permanent, should not retry
var (
	// ErrSupplierNotFound indicates the supplier state was not found.
	ErrSupplierNotFound = errors.New("supplier state not found")
)

// Session errors - permanent, should not retry
var (
	// ErrSessionTerminal indicates the session is in a terminal state.
	ErrSessionTerminal = errors.New("session is in terminal state")
)

// IsRetryableError returns true if the error is transient and should be retried.
// Only Redis connection/pool errors are considered retryable.
// All other errors (SMST logic errors, session state errors) are permanent.
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Check for Redis transient errors
	if errors.Is(err, redis.ErrClosed) {
		return true
	}

	// Redis OOM is transient — it clears when TTL-bearing keys expire
	if redisutil.IsOOMError(err) {
		return true
	}

	// Note: go-redis v9 may not export all pool errors directly,
	// but connection-related errors typically wrap net.Error
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Network errors (timeout, connection refused, etc.) are transient
		return true
	}

	// Check for context errors (timeout, canceled) - these are transient
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}

	// Everything else is considered permanent - don't retry
	return false
}

// IsPermanentSMSTError returns true if the error is a permanent SMST error
// that should result in the relay being discarded (ACKed without processing).
func IsPermanentSMSTError(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, ErrSessionSealing) ||
		errors.Is(err, ErrSessionClaimed) ||
		errors.Is(err, ErrSMSTUpdateFailed) ||
		errors.Is(err, ErrSMSTCommitFailed) ||
		errors.Is(err, ErrSessionTerminal)
}
