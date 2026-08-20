package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// lockReleaseTimeout bounds the release itself. It is short: the lock already
// carries a TTL, so a release that cannot complete quickly is better abandoned
// than left holding a connection.
const lockReleaseTimeout = time.Second

// releaseIfOwner deletes the key only when it still holds the exact token the
// caller wrote. SetNX plus DEL is not a lock: between acquiring and releasing,
// the TTL can expire and a DIFFERENT instance can acquire, and an unconditional
// DEL then frees a lock somebody else is holding -- letting a third instance in
// to fire the duplicate chain query the lock exists to prevent.
//
// That window is not theoretical here. The release runs on a context detached
// from the request precisely so it survives cancellation, so a request whose L3
// query outran the lock TTL now DOES reach Redis on its way out, where before
// it failed and left the successor alone. Making the release conditional is
// what keeps that fix from trading one duplicate query for another.
var releaseIfOwner = redis.NewScript(`
if redis.call("get", KEYS[1]) == ARGV[1] then
	return redis.call("del", KEYS[1])
end
return 0
`)

// newLockToken returns a value unique to one acquisition.
func newLockToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not recoverable here, and a CONSTANT fallback
		// is the trap: two instances both taking it would hold the same token,
		// and releaseIfOwner would let each delete the other's lock — the exact
		// thing the token exists to prevent. So the fallback still has to be
		// unique, just not random: pid and nanosecond clock cannot collide
		// across live processes on one host, and across hosts the lock is
		// contended only through Redis, where the pid pair would have to
		// coincide to the nanosecond.
		return fmt.Sprintf("fallback-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// releaseCacheLock releases a distributed cache lock this instance holds.
//
// It runs on a context DETACHED from the caller's. Every one of these locks is
// taken on a request context and released in a defer, so when the client
// disconnects or the request times out, the deferred release inherits an
// already-cancelled context, fails, and the lock sits there until its TTL
// expires. During that window every other instance asking for the same key
// takes the contended path: it sleeps, retries L2, and if the entry is not
// there yet fires the duplicate chain query the lock exists to prevent. The
// blast radius is bounded by the TTL rather than unbounded, which is why this
// went unnoticed, but a cancelled request is the NORMAL case under load.
//
// context.WithoutCancel keeps the caller's VALUES -- tracing and the like. It
// does not keep deadlines: the returned context reports none, which is why the
// timeout below is imposed here rather than inherited.
func releaseCacheLock(ctx context.Context, redisClient *redisutil.Client, lockKey, token string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), lockReleaseTimeout)
	defer cancel()
	_ = releaseIfOwner.Run(releaseCtx, redisClient, []string{lockKey}, token).Err()
}
