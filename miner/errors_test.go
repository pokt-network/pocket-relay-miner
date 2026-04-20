package miner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

func TestIsRetryableError(t *testing.T) {
	// Create a real OOM error matching Redis format (trailing space required)
	oomErr := errors.New("OOM command not allowed when used memory > 'maxmemory'")

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "Redis OOM error is retryable",
			err:  oomErr,
			want: true,
		},
		{
			name: "ErrSMSTCommitFailed-wrapped OOM is retryable (overlap case)",
			err:  fmt.Errorf("%w: flush pipeline: %v", ErrSMSTCommitFailed, oomErr),
			want: true,
		},
		{
			// context.Canceled MUST NOT be retryable: on graceful shutdown
			// the worker context is cancelled, every in-flight handleRelay
			// would return an error that never retries (the consumer is
			// exiting) and the stream message would sit in XPENDING. The
			// handleRelay call site uses IsShutdownCancelError to ACK-and-
			// discard instead — the message is re-delivered on restart.
			name: "context.Canceled is NOT retryable (shutdown classifier handles it)",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "wrapped context.Canceled is NOT retryable",
			err:  fmt.Errorf("operation failed: %w", context.Canceled),
			want: false,
		},
		{
			// DeadlineExceeded stays retryable: it means an upstream call
			// timed out, and a fresh attempt (by this or a reclaim
			// consumer) may succeed.
			name: "context.DeadlineExceeded is retryable",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "wrapped context.DeadlineExceeded is retryable",
			err:  fmt.Errorf("op failed: %w", context.DeadlineExceeded),
			want: true,
		},
		{
			name: "redis.ErrClosed is retryable",
			err:  redis.ErrClosed,
			want: true,
		},
		{
			name: "wrapped redis.ErrClosed is retryable",
			err:  fmt.Errorf("commit failed: %w", redis.ErrClosed),
			want: true,
		},
		{
			name: "net.Error (timeout) is retryable",
			err:  &net.DNSError{IsTimeout: true},
			want: true,
		},
		{
			name: "plain string error is not retryable",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "ErrSessionSealing is not retryable",
			err:  ErrSessionSealing,
			want: false,
		},
		{
			name: "ErrSessionClaimed is not retryable",
			err:  ErrSessionClaimed,
			want: false,
		},
		{
			name: "ErrSMSTCommitFailed alone is not retryable",
			err:  ErrSMSTCommitFailed,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsRetryableError(tt.err)
			if got != tt.want {
				t.Errorf("IsRetryableError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsPermanentSMSTError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "ErrSessionSealing is permanent",
			err:  ErrSessionSealing,
			want: true,
		},
		{
			name: "ErrSessionClaimed is permanent",
			err:  ErrSessionClaimed,
			want: true,
		},
		{
			name: "ErrSMSTUpdateFailed is permanent",
			err:  ErrSMSTUpdateFailed,
			want: true,
		},
		{
			name: "ErrSMSTCommitFailed is permanent",
			err:  ErrSMSTCommitFailed,
			want: true,
		},
		{
			name: "ErrSessionTerminal is permanent",
			err:  ErrSessionTerminal,
			want: true,
		},
		{
			name: "wrapped ErrSessionSealing is permanent",
			err:  fmt.Errorf("update tree: %w", ErrSessionSealing),
			want: true,
		},
		{
			name: "plain error is not permanent",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "Redis OOM is not permanent",
			err:  errors.New("OOM command not allowed when used memory > 'maxmemory'"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsPermanentSMSTError(tt.err)
			if got != tt.want {
				t.Errorf("IsPermanentSMSTError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestIsShutdownCancelError verifies the shutdown classifier used by the
// relay hot path to distinguish graceful-shutdown cancellations (ACK-and-
// discard) from upstream timeouts (retry). DeadlineExceeded wrapping
// context.Canceled should NOT be misclassified as shutdown.
func TestIsShutdownCancelError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "context.Canceled is shutdown cancel",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped context.Canceled is shutdown cancel",
			err:  fmt.Errorf("update tree: %w", context.Canceled),
			want: true,
		},
		{
			name: "deeply wrapped context.Canceled is shutdown cancel",
			err:  fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", context.Canceled)),
			want: true,
		},
		{
			name: "context.DeadlineExceeded is NOT shutdown cancel",
			err:  context.DeadlineExceeded,
			want: false,
		},
		{
			name: "wrapped context.DeadlineExceeded is NOT shutdown cancel",
			err:  fmt.Errorf("timeout: %w", context.DeadlineExceeded),
			want: false,
		},
		{
			// Both sentinels present: deadline wins (treat as retryable
			// timeout, not as a shutdown ACK-and-discard).
			name: "DeadlineExceeded wrapping Canceled is NOT shutdown cancel",
			err:  fmt.Errorf("deadline: %w: also: %w", context.DeadlineExceeded, context.Canceled),
			want: false,
		},
		{
			name: "unrelated error is NOT shutdown cancel",
			err:  errors.New("oom command not allowed"),
			want: false,
		},
		{
			name: "ErrSMSTCommitFailed wrapping Canceled is shutdown cancel",
			err:  fmt.Errorf("%w: %w", ErrSMSTCommitFailed, context.Canceled),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsShutdownCancelError(tt.err)
			if got != tt.want {
				t.Errorf("IsShutdownCancelError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestErrorClassificationOverlap verifies that the overlap case
// (OOM wrapped with ErrSMSTCommitFailed) is classified as retryable,
// which is the correct behavior after Fix #3.
func TestErrorClassificationOverlap(t *testing.T) {
	oomErr := errors.New("OOM command not allowed when used memory > 'maxmemory'")

	// This is the exact error produced by smst_manager.go FlushPipeline failure
	overlapErr := fmt.Errorf("%w: flush pipeline: %v", ErrSMSTCommitFailed, oomErr)

	// Verify it matches BOTH classifiers
	if !IsPermanentSMSTError(overlapErr) {
		t.Error("expected overlap error to match IsPermanentSMSTError (via errors.Is on ErrSMSTCommitFailed)")
	}
	if !IsRetryableError(overlapErr) {
		t.Error("expected overlap error to match IsRetryableError (via OOM string detection)")
	}

	// Verify IsOOMError detects it in the formatted string
	if !redisutil.IsOOMError(overlapErr) {
		t.Error("expected overlap error to be detected as OOM")
	}
}
