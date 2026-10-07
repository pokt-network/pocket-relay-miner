package miner

import (
	"context"
	"errors"
	"time"

	"github.com/pokt-network/smt/kvstore"
	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// smstRecord names one piece of a session tree's stored state, besides its
// nodes.
type smstRecord int

const (
	// smstNodes is the tree's nodes; only exists, expire and delete apply.
	smstNodes smstRecord = iota
	// smstClaimedRoot is the sealed root the claim was built from.
	smstClaimedRoot
	// smstStats is "count:sum" of the sealed tree.
	smstStats
	// smstLiveRoot is the checkpoint of a tree still taking relays.
	smstLiveRoot
	// smstLeaves is the leaves blob of a cold-compacted tree.
	smstLeaves
)

// smstAllRecords is every record of a session, nodes included.
var smstAllRecords = []smstRecord{smstClaimedRoot, smstLiveRoot, smstStats, smstNodes, smstLeaves}

// errSMSTRecordAbsent is what get returns for a record that is not stored.
var errSMSTRecordAbsent = errors.New("smst record absent")

// smstNodeStore is a session tree's node store: the MapStore the trie reads and
// writes, buffered between BeginPipeline and FlushPipeline.
type smstNodeStore interface {
	kvstore.MapStore
	// BeginPipeline buffers Set and Delete until FlushPipeline.
	BeginPipeline()
	// FlushPipeline writes the buffered nodes; the orphaned ones are kept for
	// FlushOrphansWithLiveRoot.
	FlushPipeline() error
	// FlushOrphansWithLiveRoot writes any node still buffered, then in one
	// step deletes the orphaned nodes and stores liveRoot, refreshing the TTL
	// of the nodes and of live_root when cacheTTL is not zero.
	FlushOrphansWithLiveRoot(ctx context.Context, liveRoot []byte, cacheTTL time.Duration) error
	// RangeNodes calls fn with every stored node.
	RangeNodes(ctx context.Context, fn func(field string, node []byte, storedBytes int) error) error
}

// smstStore keeps one supplier's session trees: their nodes and records. The
// SMST manager reaches its storage only through it.
type smstStore interface {
	// nodes is the session tree's node store.
	nodes(ctx context.Context, sessionID string) smstNodeStore
	// get reads a record; errSMSTRecordAbsent when it is not stored.
	get(ctx context.Context, rec smstRecord, sessionID string) ([]byte, error)
	// head reads at most n bytes from the start of a record; empty when it is
	// not stored.
	head(ctx context.Context, rec smstRecord, sessionID string, n int) ([]byte, error)
	// set writes a record; ttl 0 means none.
	set(ctx context.Context, rec smstRecord, sessionID string, value []byte, ttl time.Duration) error
	// exists reports whether a record is stored.
	exists(ctx context.Context, rec smstRecord, sessionID string) (bool, error)
	// expire sets a record's TTL; a record that is not stored is left alone.
	expire(ctx context.Context, rec smstRecord, sessionID string, ttl time.Duration) error
	// del deletes records of the session, returning how many existed.
	del(ctx context.Context, sessionID string, recs ...smstRecord) (int64, error)
	// compacted reports whether the session's tree is stored as its leaves
	// only: the leaves blob present and the nodes absent.
	compacted(ctx context.Context, sessionID string) (bool, error)
	// setLiveRootIfUnchanged stores root as live_root only while live_root
	// still holds expected (nil meaning absent), and refreshes the TTL of it
	// and of the nodes as FlushOrphansWithLiveRoot does, deleting no node.
	setLiveRootIfUnchanged(ctx context.Context, sessionID string, root, expected []byte, ttl time.Duration) (bool, error)
	// deleteNodesIfLeaves deletes the session's nodes only while its leaves
	// blob is still leaves, so two compactions of one session cannot leave it
	// with neither.
	deleteNodesIfLeaves(ctx context.Context, sessionID string, leaves []byte) (bool, error)
	// sessionsWithNodes lists the sessions whose nodes are stored.
	sessionsWithNodes(ctx context.Context) ([]string, error)
}

// redisSMSTStore is the smstStore over Redis: a hash of nodes and one key per
// record, every key built by the KeyBuilder for (supplier, session).
type redisSMSTStore struct {
	client   *redisutil.Client
	supplier string
}

func newRedisSMSTStore(client *redisutil.Client, supplier string) *redisSMSTStore {
	return &redisSMSTStore{client: client, supplier: supplier}
}

func (s *redisSMSTStore) key(rec smstRecord, sessionID string) string {
	kb := s.client.KB()
	switch rec {
	case smstClaimedRoot:
		return kb.SMSTRootKey(s.supplier, sessionID)
	case smstStats:
		return kb.SMSTStatsKey(s.supplier, sessionID)
	case smstLiveRoot:
		return kb.SMSTLiveRootKey(s.supplier, sessionID)
	case smstLeaves:
		return kb.SMSTLeavesKey(s.supplier, sessionID)
	default:
		return kb.SMSTNodesKey(s.supplier, sessionID)
	}
}

func (s *redisSMSTStore) nodes(ctx context.Context, sessionID string) smstNodeStore {
	return newRedisMapStore(ctx, s.client, s.supplier, sessionID)
}

func (s *redisSMSTStore) get(ctx context.Context, rec smstRecord, sessionID string) ([]byte, error) {
	value, err := s.client.Get(ctx, s.key(rec, sessionID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, errSMSTRecordAbsent
	}
	return value, err
}

func (s *redisSMSTStore) head(ctx context.Context, rec smstRecord, sessionID string, n int) ([]byte, error) {
	return s.client.GetRange(ctx, s.key(rec, sessionID), 0, int64(n)-1).Bytes()
}

func (s *redisSMSTStore) set(ctx context.Context, rec smstRecord, sessionID string, value []byte, ttl time.Duration) error {
	return s.client.Set(ctx, s.key(rec, sessionID), value, ttl).Err()
}

func (s *redisSMSTStore) exists(ctx context.Context, rec smstRecord, sessionID string) (bool, error) {
	n, err := s.client.Exists(ctx, s.key(rec, sessionID)).Result()
	return n == 1, err
}

func (s *redisSMSTStore) expire(ctx context.Context, rec smstRecord, sessionID string, ttl time.Duration) error {
	return s.client.Expire(ctx, s.key(rec, sessionID), ttl).Err()
}

func (s *redisSMSTStore) del(ctx context.Context, sessionID string, recs ...smstRecord) (int64, error) {
	keys := make([]string, len(recs))
	for i, rec := range recs {
		keys[i] = s.key(rec, sessionID)
	}
	return s.client.Del(ctx, keys...).Result()
}

func (s *redisSMSTStore) compacted(ctx context.Context, sessionID string) (bool, error) {
	pipe := s.client.Pipeline()
	nodes := pipe.Exists(ctx, s.key(smstNodes, sessionID))
	blob := pipe.Exists(ctx, s.key(smstLeaves, sessionID))
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	return blob.Val() == 1 && nodes.Val() == 0, nil
}

func (s *redisSMSTStore) setLiveRootIfUnchanged(ctx context.Context, sessionID string, root, expected []byte, ttl time.Duration) (bool, error) {
	keys := []string{s.key(smstLiveRoot, sessionID), s.key(smstNodes, sessionID)}
	set, err := exitLiveRootScript.Run(ctx, s.client, keys, root, expected, int64(ttl.Seconds())).Int64()
	return set == 1, err
}

func (s *redisSMSTStore) deleteNodesIfLeaves(ctx context.Context, sessionID string, leaves []byte) (bool, error) {
	keys := []string{s.key(smstNodes, sessionID), s.key(smstLeaves, sessionID)}
	deleted, err := unlinkNodesIfBlobScript.Run(ctx, s.client, keys, leaves).Int64()
	return deleted == 1, err
}

func (s *redisSMSTStore) sessionsWithNodes(ctx context.Context) ([]string, error) {
	kb := s.client.KB()
	prefix := kb.SMSTNodesPrefix() + s.supplier + ":"
	var sessions []string
	var cursor uint64
	for {
		keys, next, err := s.client.Scan(ctx, cursor, kb.SMSTNodesPattern(), RedisScanBatchSize).Result()
		if err != nil {
			return sessions, err
		}
		for _, key := range keys {
			if id, ok := smstSessionOfNodesKey(key, prefix); ok {
				sessions = append(sessions, id)
			}
		}
		cursor = next
		if cursor == 0 {
			return sessions, nil
		}
	}
}

// smstSessionOfNodesKey parses "<prefix><session>:nodes" where prefix ends with
// the supplier and a colon; false for any other shape. The session ID may hold
// colons, as the warmup's parse always allowed.
func smstSessionOfNodesKey(key, prefix string) (string, bool) {
	const suffix = ":nodes"
	if len(key) <= len(prefix)+len(suffix) || key[:len(prefix)] != prefix || key[len(key)-len(suffix):] != suffix {
		return "", false
	}
	return key[len(prefix) : len(key)-len(suffix)], true
}

var _ smstStore = (*redisSMSTStore)(nil)
var _ smstNodeStore = (*nodeStore)(nil)
