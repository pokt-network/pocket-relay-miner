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

	// ErrSMSTNodeMissing indicates a tree node referenced by an inner node
	// is absent from the backing store. Returned by RedisMapStore.Get instead
	// of (nil, nil) when corruption is detected, so the smt library
	// propagates the error up through Update/Commit/ProveClosest rather
	// than panicking inside parseSumTrieNode on a zero-length slice.
	ErrSMSTNodeMissing = errors.New("SMST node missing from store")

	// ErrSMSTPanicRecovered is returned when a defer/recover at the miner
	// boundary intercepts a panic from the smt library. The recovered
	// panic value is wrapped with %w so callers can still inspect it.
	ErrSMSTPanicRecovered = errors.New("SMST library panic recovered")

	// ErrRelayPanicRecovered marks a relay whose processing panicked and was
	// recovered. It is a SENTINEL rather than a bare fmt.Errorf because the
	// decision it drives is binary and consequential: a panic is deterministic,
	// so handing the entry back for another delivery just repeats it, while
	// every other processing failure deserves the retry. errors.Is is the only
	// honest way to ask that question -- matching on the message text would
	// break the day someone rewords it.
	ErrRelayPanicRecovered = errors.New("relay processing panic recovered")
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
// Only Redis connection/pool errors and upstream timeouts are considered
// retryable. All other errors (SMST logic errors, session state errors,
// shutdown-origin cancellations) are permanent.
//
// context.Canceled is deliberately NOT retryable: on graceful shutdown or
// supplier removal the worker context is cancelled, every in-flight
// handleRelay sees context.Canceled through Redis call sites, and
// classifying it as retryable would leave the stream message permanently
// pending in XPENDING (the consumer is about to exit and will not retry).
// The same message is re-delivered by XREADGROUP on restart so no relay is
// lost. Shutdown-origin cancellations should be handled with
// IsShutdownCancelError at the call site (ACK-and-discard).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}

	// Check for Redis transient errors
	if errors.Is(err, redis.ErrClosed) {
		return true
	}

	// A pool timeout means every connection was busy for longer than
	// PoolTimeout. It is transient by construction -- the next delivery
	// finds a free connection -- but nothing else in this function can see
	// it: redis.ErrPoolTimeout is a bare errors.New (go-redis v9.22.0
	// internal/pool/pool.go:65), so it implements neither net.Error nor
	// Timeout(), and it wraps no context error. Without this check the flush
	// error reaches IsPermanentSMSTError through its ErrSMSTCommitFailed
	// wrapper (smst_manager.go:679) and handleRelay ACKs a relay that was
	// already served and signed, deleting it from the stream with DELREF and
	// counting it as "session_sealed" -- money lost, labelled "arrived late".
	//
	// go-redis itself classifies this error as retryable in its own retry
	// loop (v9.22.0 error.go:107-111, "connection pool timeout, increase
	// retries. #3289"), so treating it as permanent here disagrees with the
	// library that produces it.
	if errors.Is(err, redis.ErrPoolTimeout) {
		return true
	}

	// Redis OOM is transient — it clears when TTL-bearing keys expire
	if redisutil.IsOOMError(err) {
		return true
	}

	// Network errors reach us as net.Error. This does NOT cover the pool
	// sentinels -- those are bare errors.New values and are checked by
	// identity above -- so a new pool error class needs its own errors.Is,
	// not a hope that it wraps net.Error.
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Network errors (timeout, connection refused, etc.) are transient
		return true
	}

	// Upstream timeouts are retryable (a later attempt may succeed).
	// context.Canceled is NOT retryable — see function doc.
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Everything else is considered permanent - don't retry
	return false
}

// IsShutdownCancelError returns true when err wraps context.Canceled
// without also wrapping context.DeadlineExceeded. It is used by the relay
// hot path to classify shutdown-origin cancellations: the correct action
// is ACK-and-discard (the same stream message is re-delivered by
// XREADGROUP to the next consumer after restart), not retry-forever.
//
// Callers should ALSO pass the calling goroutine's ctx.Err() through this
// classifier: when the supplier worker is shutting down, ctx is cancelled
// and any wrapped error we see is almost certainly shutdown-origin.
func IsShutdownCancelError(err error) bool {
	if err == nil {
		return false
	}
	// DeadlineExceeded wins if both are present — treat as a timeout
	// (retryable) rather than a shutdown cancel.
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, context.Canceled)
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
		errors.Is(err, ErrSMSTNodeMissing) ||
		errors.Is(err, ErrSMSTPanicRecovered) ||
		errors.Is(err, ErrSessionTerminal)
}
