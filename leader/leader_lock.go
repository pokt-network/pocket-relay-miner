package leader

import (
	"context"
	"sync"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// leaderLock holds the global leadership lock. Each call answers 1 when it
// did what it names and 0 when the lock is someone else's (acquire: already
// held), as the Lua scripts do.
type leaderLock interface {
	acquire(ctx context.Context, owner string, ttlSeconds int) (int, error)
	renew(ctx context.Context, owner string, ttlSeconds int) (int, error)
	release(ctx context.Context, owner string) (int, error)
}

// redisLock is the lock as a Redis key with a TTL, shared by every replica.
type redisLock struct {
	client        *redisutil.Client
	key           string // built via KeyBuilder
	acquireScript *redis.Script
	renewScript   *redis.Script
	releaseScript *redis.Script
}

func (l *redisLock) acquire(ctx context.Context, owner string, ttlSeconds int) (int, error) {
	return l.acquireScript.Run(ctx, l.client, []string{l.key}, owner, ttlSeconds).Int()
}

func (l *redisLock) renew(ctx context.Context, owner string, ttlSeconds int) (int, error) {
	return l.renewScript.Run(ctx, l.client, []string{l.key}, owner, ttlSeconds).Int()
}

func (l *redisLock) release(ctx context.Context, owner string) (int, error) {
	return l.releaseScript.Run(ctx, l.client, []string{l.key}, owner).Int()
}

// exclusiveLock is the lock of a process with no peers: held in memory, it
// cannot fail, expire or outlive the process, so a restarted process holds it
// at once.
type exclusiveLock struct {
	mu    sync.Mutex
	owner string
}

func (l *exclusiveLock) acquire(_ context.Context, owner string, _ int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != "" {
		return 0, nil // held, as EXISTS answers whoever holds it
	}
	l.owner = owner
	return 1, nil
}

func (l *exclusiveLock) renew(_ context.Context, owner string, _ int) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != owner {
		return 0, nil
	}
	return 1, nil
}

func (l *exclusiveLock) release(_ context.Context, owner string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.owner != owner {
		return 0, nil
	}
	l.owner = ""
	return 1, nil
}

var (
	_ leaderLock = (*redisLock)(nil)
	_ leaderLock = (*exclusiveLock)(nil)
)
