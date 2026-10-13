package miner

import (
	"context"
	"errors"
)

// errCommitRefused is what a relayCommitter returns when a session's batch must
// not be retried as a batch: the batch then finishes those relays one at a
// time. A committer returns it only for a refusal made before any write to the
// dedup marks, counters or entries.
var errCommitRefused = errors.New("relay batch: commit refused")

// relayCommitter finishes what relayBatch holds, in the store the relays'
// queue, dedup marks and session counters live in.
type relayCommitter interface {
	// CommitSession is called only once the session's tree is reachable from a
	// stored root covering every relay passed (the live_root checkpoint): an
	// acknowledged entry is never delivered again.
	//
	// It does, as ONE unit that no other writer of the same dedup marks,
	// counters or entries can interleave with:
	//  1. marks each relay's hash in the session's dedup set;
	//  2. if the session exists and is not terminal, adds to its counters
	//     exactly the relays whose mark is new, each with its OWN compute units;
	//  3. acknowledges and removes every relay's entry.
	//
	// The result's status is 0 counted, 1 session missing, 2 session terminal;
	// a missing or terminal session still gets 1 and 3. freshDups counts the
	// relays already marked whose entry this call removed: copies delivered
	// after another consumer finished them.
	//
	// An error wrapping errCommitRefused: see errCommitRefused. Any other error
	// means the outcome is unknown: the caller retries the same input, and a
	// run after an unreported success marks nothing new, counts nothing and
	// reports no freshDups.
	CommitSession(ctx context.Context, sessionID string, relays []batchedRelay) (relayBatchResult, error)

	// AckRejected acknowledges and removes entries for which nothing is marked
	// or counted. An error leaves every one of them pending; retrying is safe.
	AckRejected(ctx context.Context, ids []string) error
}
