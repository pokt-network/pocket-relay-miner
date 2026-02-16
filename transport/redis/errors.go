package redis

import "strings"

// IsOOMError returns true if the error is a Redis OOM (Out of Memory) error.
// Redis returns "OOM command not allowed when used memory > 'maxmemory'" when
// it cannot execute write commands due to memory limits.
//
// OOM errors are transient — they clear when TTL-bearing keys expire or when
// memory is freed through eviction policies. Callers should treat these as
// retryable errors.
func IsOOMError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "OOM")
}
