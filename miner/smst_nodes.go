package miner

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// nodeBackend stores one session tree's nodes, each under its field (the hex
// of the node's key) and already encoded by the node codec. nodeStore keeps
// the buffers and the orphan rules above it.
type nodeBackend interface {
	// name identifies the nodes in errors.
	name() string
	// get reads a node; found is false when it is not stored.
	get(ctx context.Context, field string) (stored []byte, found bool, err error)
	// put writes nodes, all or none from the caller's point of view: on an
	// error the caller writes them all again.
	put(ctx context.Context, fields []string, stored [][]byte) error
	// del deletes a node.
	del(ctx context.Context, field string) error
	// count is the number of nodes stored.
	count(ctx context.Context) (int, error)
	// clear deletes every node.
	clear(ctx context.Context) error
	// scan calls fn with every stored node; a node may be passed twice.
	scan(ctx context.Context, fn func(field string, stored []byte) error) error
	// checkpoint deletes the orphaned nodes and stores the live root in one
	// step, refreshing the TTL of the nodes and of the live root when ttl is
	// not zero.
	checkpoint(ctx context.Context, orphans []string, liveRoot []byte, ttl time.Duration) error
}

// redisNodes keeps a tree's nodes in one Redis hash.
type redisNodes struct {
	client      *redisutil.Client
	hashKey     string
	liveRootKey string
}

func (r *redisNodes) name() string { return r.hashKey }

func (r *redisNodes) get(ctx context.Context, field string) ([]byte, bool, error) {
	stored, err := r.client.HGet(ctx, r.hashKey, field).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	return stored, err == nil, err
}

// put sends the nodes as HSETs of at most nodesWriteChunkBytes each, in one
// round trip; the pieces that did land before an error are written again by
// the caller's retry, which HSET makes harmless.
func (r *redisNodes) put(ctx context.Context, fields []string, stored [][]byte) error {
	var chunks [][]interface{}
	args := make([]interface{}, 0, len(fields)*2)
	chunkBytes := 0
	for i, field := range fields {
		if len(args) > 0 && chunkBytes+len(field)+len(stored[i]) > nodesWriteChunkBytes {
			chunks = append(chunks, args)
			args = make([]interface{}, 0, len(fields)*2-len(args))
			chunkBytes = 0
		}
		args = append(args, field, stored[i])
		chunkBytes += len(field) + len(stored[i])
	}
	chunks = append(chunks, args)
	if len(chunks) == 1 {
		return r.client.HSet(ctx, r.hashKey, chunks[0]...).Err()
	}
	_, err := r.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, chunk := range chunks {
			pipe.HSet(ctx, r.hashKey, chunk...)
		}
		return nil
	})
	return err
}

func (r *redisNodes) del(ctx context.Context, field string) error {
	return r.client.HDel(ctx, r.hashKey, field).Err()
}

func (r *redisNodes) count(ctx context.Context) (int, error) {
	n, err := r.client.HLen(ctx, r.hashKey).Result()
	return int(n), err
}

func (r *redisNodes) clear(ctx context.Context) error {
	return r.client.Del(ctx, r.hashKey).Err()
}

// scan walks the hash with HSCAN, which may return a field twice.
func (r *redisNodes) scan(ctx context.Context, fn func(field string, stored []byte) error) error {
	var cursor uint64
	for {
		kvs, next, err := r.client.HScan(ctx, r.hashKey, cursor, "", coldLeavesScanCount).Result()
		if err != nil {
			return err
		}
		for i := 0; i+1 < len(kvs); i += 2 {
			if err := fn(kvs[i], []byte(kvs[i+1])); err != nil {
				return err
			}
		}
		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

// checkpoint is one MULTI/EXEC: the orphans' HDEL, the live_root SET, and the
// sliding TTL on both keys, so live_root never outlives the nodes it
// references.
func (r *redisNodes) checkpoint(ctx context.Context, orphans []string, liveRoot []byte, ttl time.Duration) error {
	pipe := r.client.TxPipeline()
	if len(orphans) > 0 {
		pipe.HDel(ctx, r.hashKey, orphans...)
	}
	pipe.Set(ctx, r.liveRootKey, liveRoot, 0)
	if ttl > 0 {
		pipe.Expire(ctx, r.hashKey, ttl)
		pipe.Expire(ctx, r.liveRootKey, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

var _ nodeBackend = (*redisNodes)(nil)
