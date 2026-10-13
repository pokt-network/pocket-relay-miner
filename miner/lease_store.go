package miner

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// errLeaseAbsent is what owner returns for a supplier no instance holds.
var errLeaseAbsent = errors.New("lease absent")

// leaseStore holds the supplier leases and the set of live miner instances the
// SupplierClaimer shares them out over. The claimer's state machine -- claim,
// adopt, renew, drain, reap -- is the same over every store.
type leaseStore interface {
	// leaseKey names a supplier's lease in logs.
	leaseKey(supplier string) string
	// setNX takes the lease if no one holds it.
	setNX(ctx context.Context, supplier, owner string, ttl time.Duration) (bool, error)
	// owner is who holds the lease; errLeaseAbsent when no one does.
	owner(ctx context.Context, supplier string) (string, error)
	// expire sets the lease's TTL; false when no one holds it.
	expire(ctx context.Context, supplier string, ttl time.Duration) (bool, error)
	// releaseIfOwner deletes the lease only while owner holds it, and reports
	// whether it did.
	releaseIfOwner(ctx context.Context, supplier, owner string) (bool, error)
	// extendIfOwner sets the lease's TTL only while owner holds it.
	extendIfOwner(ctx context.Context, supplier, owner string, ttl time.Duration) error
	// exists reports whether anyone holds the lease.
	exists(ctx context.Context, supplier string) (bool, error)

	// setInstance records that instance is alive, for ttl.
	setInstance(ctx context.Context, instance string, ttl time.Duration) error
	// addActive and removeActive maintain the set of active instances.
	addActive(ctx context.Context, instance string) error
	removeActive(ctx context.Context, instance string)
	// deleteInstance forgets that instance is alive.
	deleteInstance(ctx context.Context, instance string)
	// activeCount is the size of the active set.
	activeCount(ctx context.Context) (int64, error)
	// activeMembers is the active set.
	activeMembers(ctx context.Context) ([]string, error)
	// instanceAlive reports whether instance's liveness record exists.
	instanceAlive(ctx context.Context, instance string) (bool, error)
}

// redisLeaseStore is the lease store every replica shares: one key per lease,
// one per live instance, and a set of active instances.
type redisLeaseStore struct {
	client *redisutil.Client
}

// releaseLeaseScript deletes a lease only if this instance still holds it.
var releaseLeaseScript = redis.NewScript(`
	if redis.call("get", KEYS[1]) == ARGV[1] then
		return redis.call("del", KEYS[1])
	else
		return 0
	end
`)

// extendDrainLeaseScript sets a lease's TTL, in milliseconds, only if this
// instance still holds it.
var extendDrainLeaseScript = redis.NewScript(`
	if redis.call("get", KEYS[1]) == ARGV[1] then
		return redis.call("pexpire", KEYS[1], ARGV[2])
	else
		return 0
	end
`)

func (s *redisLeaseStore) leaseKey(supplier string) string {
	return s.client.KB().MinerClaimKey(supplier)
}

func (s *redisLeaseStore) setNX(ctx context.Context, supplier, owner string, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, s.leaseKey(supplier), owner, ttl).Result()
}

func (s *redisLeaseStore) owner(ctx context.Context, supplier string) (string, error) {
	owner, err := s.client.Get(ctx, s.leaseKey(supplier)).Result()
	if err == redis.Nil {
		return "", errLeaseAbsent
	}
	return owner, err
}

func (s *redisLeaseStore) expire(ctx context.Context, supplier string, ttl time.Duration) (bool, error) {
	return s.client.Expire(ctx, s.leaseKey(supplier), ttl).Result()
}

func (s *redisLeaseStore) releaseIfOwner(ctx context.Context, supplier, owner string) (bool, error) {
	result, err := releaseLeaseScript.Run(ctx, s.client, []string{s.leaseKey(supplier)}, owner).Int64()
	return result != 0, err
}

func (s *redisLeaseStore) extendIfOwner(ctx context.Context, supplier, owner string, ttl time.Duration) error {
	return extendDrainLeaseScript.Run(ctx, s.client, []string{s.leaseKey(supplier)}, owner, ttl.Milliseconds()).Err()
}

func (s *redisLeaseStore) exists(ctx context.Context, supplier string) (bool, error) {
	n, err := s.client.Exists(ctx, s.leaseKey(supplier)).Result()
	return n != 0, err
}

func (s *redisLeaseStore) setInstance(ctx context.Context, instance string, ttl time.Duration) error {
	return s.client.Set(ctx, s.client.KB().MinerInstanceKey(instance), time.Now().UnixNano(), ttl).Err()
}

func (s *redisLeaseStore) addActive(ctx context.Context, instance string) error {
	return s.client.SAdd(ctx, s.client.KB().MinerActiveSetKey(), instance).Err()
}

func (s *redisLeaseStore) removeActive(ctx context.Context, instance string) {
	s.client.SRem(ctx, s.client.KB().MinerActiveSetKey(), instance)
}

func (s *redisLeaseStore) deleteInstance(ctx context.Context, instance string) {
	s.client.Del(ctx, s.client.KB().MinerInstanceKey(instance))
}

func (s *redisLeaseStore) activeCount(ctx context.Context) (int64, error) {
	return s.client.SCard(ctx, s.client.KB().MinerActiveSetKey()).Result()
}

func (s *redisLeaseStore) activeMembers(ctx context.Context) ([]string, error) {
	return s.client.SMembers(ctx, s.client.KB().MinerActiveSetKey()).Result()
}

func (s *redisLeaseStore) instanceAlive(ctx context.Context, instance string) (bool, error) {
	n, err := s.client.Exists(ctx, s.client.KB().MinerInstanceKey(instance)).Result()
	return n != 0, err
}

// exclusiveLeaseStore is the lease store of a process with no peers
// (standalone): held in memory, it cannot fail or outlive the process, the
// active set is this process alone, and so the claimer takes every supplier.
// TTLs are not kept: nothing else could take a lease when one ran out.
type exclusiveLeaseStore struct {
	mu        sync.Mutex
	leases    map[string]string
	instances map[string]struct{}
	active    map[string]struct{}
}

func newExclusiveLeaseStore() *exclusiveLeaseStore {
	return &exclusiveLeaseStore{
		leases:    make(map[string]string),
		instances: make(map[string]struct{}),
		active:    make(map[string]struct{}),
	}
}

func (s *exclusiveLeaseStore) leaseKey(supplier string) string { return supplier }

func (s *exclusiveLeaseStore) setNX(_ context.Context, supplier, owner string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, held := s.leases[supplier]; held {
		return false, nil
	}
	s.leases[supplier] = owner
	return true, nil
}

func (s *exclusiveLeaseStore) owner(_ context.Context, supplier string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owner, held := s.leases[supplier]
	if !held {
		return "", errLeaseAbsent
	}
	return owner, nil
}

func (s *exclusiveLeaseStore) expire(_ context.Context, supplier string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, held := s.leases[supplier]
	return held, nil
}

func (s *exclusiveLeaseStore) releaseIfOwner(_ context.Context, supplier, owner string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[supplier] != owner {
		return false, nil
	}
	delete(s.leases, supplier)
	return true, nil
}

func (s *exclusiveLeaseStore) extendIfOwner(context.Context, string, string, time.Duration) error {
	return nil
}

func (s *exclusiveLeaseStore) exists(_ context.Context, supplier string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, held := s.leases[supplier]
	return held, nil
}

func (s *exclusiveLeaseStore) setInstance(_ context.Context, instance string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instances[instance] = struct{}{}
	return nil
}

func (s *exclusiveLeaseStore) addActive(_ context.Context, instance string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active[instance] = struct{}{}
	return nil
}

func (s *exclusiveLeaseStore) removeActive(_ context.Context, instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, instance)
}

func (s *exclusiveLeaseStore) deleteInstance(_ context.Context, instance string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.instances, instance)
}

func (s *exclusiveLeaseStore) activeCount(context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.active)), nil
}

func (s *exclusiveLeaseStore) activeMembers(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.active))
	for instance := range s.active {
		out = append(out, instance)
	}
	return out, nil
}

func (s *exclusiveLeaseStore) instanceAlive(_ context.Context, instance string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, alive := s.instances[instance]
	return alive, nil
}

var (
	_ leaseStore = (*redisLeaseStore)(nil)
	_ leaseStore = (*exclusiveLeaseStore)(nil)
)
