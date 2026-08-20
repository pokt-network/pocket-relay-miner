package cache

import (
	"context"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// lockReleaseTimeout bounds the release itself. It is short: the lock already
// carries a TTL, so a release that cannot complete quickly is better abandoned
// than left holding a connection.
const lockReleaseTimeout = time.Second

// releaseCacheLock deletes a distributed cache lock this instance holds.
//
// It runs on a context DETACHED from the caller's. Every one of these locks is
// taken on a request context, and the release is deferred -- so when the client
// disconnects or the request times out, the deferred Del inherits an
// already-cancelled context, fails, and the lock sits there until its TTL
// expires. During that window every other instance asking for the same key
// takes the contended path: it sleeps, retries L2, and if the entry is not
// there yet fires the duplicate chain query the lock exists to prevent. The
// blast radius is bounded by the TTL rather than unbounded, which is why this
// went unnoticed, but a cancelled request is the NORMAL case under load, not
// the rare one.
//
// context.WithoutCancel keeps the caller's values (tracing, deadlines the
// client library reads) while dropping the cancellation.
func releaseCacheLock(ctx context.Context, redisClient *redisutil.Client, lockKey string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
	defer cancel()
	_ = redisClient.Del(releaseCtx, lockKey).Err()
}
