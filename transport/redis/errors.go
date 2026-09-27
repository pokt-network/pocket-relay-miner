package redis

import "strings"

// IsOOMError returns true if the error is a Redis OOM (Out of Memory) error.
// Redis returns "OOM command not allowed when used memory > 'maxmemory'" when
// it cannot execute write commands due to memory limits. We match on "OOM command"
// rather than bare "OOM" to avoid false positives on strings like "ZOOM" or "ROOM".
//
// OOM errors are transient — they clear when TTL-bearing keys expire or when
// memory is freed through eviction policies. Callers should treat these as
// retryable errors.
func IsOOMError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "OOM command")
}

// IsWrongTypeError reports whether a command failed because the key already
// exists holding a different type -- Redis answers "WRONGTYPE Operation against
// a key holding the wrong kind of value".
//
// This one is PERMANENT, which is what makes it worth classifying: no number of
// retries turns a string into a stream, and a relay stream that has become a
// string stays one until an operator removes the key. Retrying it forever is
// what used to block the publisher's whole queue.
//
// Matched on the leading error code and not with a bare Contains, for the reason
// IsOOMError above matches "OOM command": the code is the first token of the
// server's reply, so anchoring there cannot be satisfied by the word appearing
// anywhere else. go-redis surfaces a server error as its raw reply string with
// no separate code field, so the text is the only thing there is to read.
func IsWrongTypeError(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(err.Error(), "WRONGTYPE ")
}
