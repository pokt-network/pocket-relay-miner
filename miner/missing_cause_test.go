//go:build test

package miner

import (
	"testing"

	"github.com/pokt-network/pocket-relay-miner/tx"
)

// TestResolveMissingCause pins the one verdict that stops being safe once an
// entry can have resent more than once.
//
// The boundary is exact and it is not off by one: with one resend there are two
// transactions and the entry holds both hashes, so the read saw everything. With
// two there are three and the middle hash was overwritten, so a NotInBlock is
// derived from a strict subset and cannot rule out that the attempt it never saw
// was included and failed.
//
// The cases that must NOT change are as much the point as the one that does: a
// fix that degraded every verdict would trade an inverted label for a blind one,
// and IncludedOK / IncludedFailed are POSITIVE observations -- one hash saying
// "I am in a block" is true no matter how many other hashes exist.
func TestResolveMissingCause(t *testing.T) {
	tests := []struct {
		name         string
		cause        tx.TxInclusion
		rebroadcasts int
		want         tx.TxInclusion
	}{
		{"no resend: both hashes are the same tx and the read saw it", tx.TxInclusionNotInBlock, 0, tx.TxInclusionNotInBlock},
		{"one resend: two txs, two hashes, nothing was missed", tx.TxInclusionNotInBlock, 1, tx.TxInclusionNotInBlock},
		{"two resends: three txs and the middle hash is gone", tx.TxInclusionNotInBlock, 2, tx.TxInclusionUnknown},
		{"three resends: still incomplete", tx.TxInclusionNotInBlock, 3, tx.TxInclusionUnknown},

		{"an included-OK verdict is a positive observation and survives", tx.TxInclusionIncludedOK, 2, tx.TxInclusionIncludedOK},
		{"an included-failed verdict is the expensive one to lose, and survives", tx.TxInclusionIncludedFailed, 2, tx.TxInclusionIncludedFailed},
		{"unknown stays unknown", tx.TxInclusionUnknown, 2, tx.TxInclusionUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveMissingCause(tt.cause, tt.rebroadcasts); got != tt.want {
				t.Errorf(
					"resolveMissingCause(%v, %d) = %v, want %v",
					tt.cause, tt.rebroadcasts, got, tt.want,
				)
			}
		})
	}
}
