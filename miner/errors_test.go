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
			name: "context.Canceled is retryable",
			err:  context.Canceled,
			want: true,
		},
		{
			name: "wrapped context.Canceled is retryable",
			err:  fmt.Errorf("operation failed: %w", context.Canceled),
			want: true,
		},
		{
			name: "context.DeadlineExceeded is retryable",
			err:  context.DeadlineExceeded,
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
