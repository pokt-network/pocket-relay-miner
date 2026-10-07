package kv

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Redis is the Store over a Redis client: each method is the one command it
// names, so behaviour is the client's own.
type Redis struct {
	client *redisutil.Client
	logger logging.Logger
}

// NewRedis returns the Store over client.
func NewRedis(logger logging.Logger, client *redisutil.Client) *Redis {
	return &Redis{client: client, logger: logger}
}

// Client is the underlying client, for the code that still needs commands
// this interface does not carry.
func (r *Redis) Client() *redisutil.Client { return r.client }

func (r *Redis) KB() *redisutil.KeyBuilder { return r.client.KB() }

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	value, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	return value, err
}

func (r *Redis) MGet(ctx context.Context, keys ...string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := r.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(vals))
	for i, v := range vals {
		if s, ok := v.(string); ok {
			out[i] = []byte(s)
		}
	}
	return out, nil
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

// SetAll is one MULTI/EXEC: every SET or none.
func (r *Redis) SetAll(ctx context.Context, entries ...Entry) error {
	if len(entries) == 0 {
		return nil
	}
	pipe := r.client.TxPipeline()
	for _, e := range entries {
		pipe.Set(ctx, e.Key, e.Value, e.TTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (r *Redis) SetKeepTTL(ctx context.Context, key string, value []byte) error {
	return r.client.Set(ctx, key, value, redis.KeepTTL).Err()
}

func (r *Redis) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return r.client.SetNX(ctx, key, value, ttl).Result()
}

// compareAndDeleteScript deletes KEYS[1] only while it holds ARGV[1].
var compareAndDeleteScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
	return redis.call('DEL', KEYS[1])
end
return 0
`)

func (r *Redis) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	n, err := compareAndDeleteScript.Run(ctx, r.client, []string{key}, expected).Int64()
	return n == 1, err
}

func (r *Redis) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return r.client.Del(ctx, keys...).Err()
}

func (r *Redis) Exists(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Exists(ctx, key).Result()
	return n == 1, err
}

func (r *Redis) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return r.client.Expire(ctx, key, ttl).Result()
}

// scanCount is the SCAN batch: one round trip per 500 keys examined, as the
// submission tracker's own scan used.
const scanCount = 500

func (r *Redis) ScanPrefix(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	iter := r.client.Scan(ctx, 0, prefix+"*", scanCount).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

func (r *Redis) SAdd(ctx context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	return r.client.SAdd(ctx, key, toAny(members)...).Err()
}

func (r *Redis) SRem(ctx context.Context, key string, members ...string) error {
	if len(members) == 0 {
		return nil
	}
	return r.client.SRem(ctx, key, toAny(members)...).Err()
}

func (r *Redis) SMembers(ctx context.Context, key string) ([]string, error) {
	return r.client.SMembers(ctx, key).Result()
}

func (r *Redis) SCard(ctx context.Context, key string) (int64, error) {
	return r.client.SCard(ctx, key).Result()
}

func (r *Redis) SIsMember(ctx context.Context, key string, member string) (bool, error) {
	return r.client.SIsMember(ctx, key, member).Result()
}

func (r *Redis) Publish(ctx context.Context, channel string, payload []byte) error {
	return r.client.Publish(ctx, channel, payload).Err()
}

func (r *Redis) Subscribe(ctx context.Context, channels ...string) (Subscription, error) {
	pubsub := r.client.Subscribe(ctx, channels...)
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, err
	}
	sub := &redisSubscription{pubsub: pubsub, out: make(chan Message, 100), done: make(chan struct{})}
	go logging.RecoverGoRoutine(r.logger, "kv_redis_subscription", func(context.Context) {
		defer close(sub.out)
		in := pubsub.Channel()
		for {
			select {
			case msg, ok := <-in:
				if !ok || msg == nil {
					return
				}
				select {
				case sub.out <- Message{Channel: msg.Channel, Payload: msg.Payload}:
				case <-sub.done:
					return
				}
			case <-sub.done:
				return
			}
		}
	})(ctx)
	return sub, nil
}

func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

type redisSubscription struct {
	pubsub    *redis.PubSub
	out       chan Message
	done      chan struct{}
	closeOnce sync.Once
}

func (s *redisSubscription) Messages() <-chan Message { return s.out }

func (s *redisSubscription) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		err = s.pubsub.Close()
	})
	return err
}

func toAny(members []string) []any {
	out := make([]any, len(members))
	for i, m := range members {
		out[i] = m
	}
	return out
}

var _ Store = (*Redis)(nil)
