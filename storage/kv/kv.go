// Package kv is the small key-value surface the caches, the relay meter and the
// miner's registries use: strings with a TTL, sets, compare-and-delete locks
// and pub/sub. It has exactly the operations those callers use, so a backend
// is a short file: Redis for the relayer and miner subcommands, the embedded
// store for standalone.
//
// Nothing here is a transaction. What has to be atomic across several keys
// (a relay batch, a session's counters) lives behind its own interface
// instead.
package kv

import (
	"context"
	"errors"
	"time"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// ErrNotFound is what Get returns for a key that does not exist.
var ErrNotFound = errors.New("kv: not found")

// Store is a key-value store. Keys are built with KB(), as for Redis.
type Store interface {
	// KB builds every key and channel name.
	KB() *redisutil.KeyBuilder

	// Get returns the key's value, or ErrNotFound.
	Get(ctx context.Context, key string) ([]byte, error)
	// MGet returns the values of keys in order, nil for a key that does not
	// exist.
	MGet(ctx context.Context, keys ...string) ([][]byte, error)
	// Set writes the value; ttl 0 means no expiry.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// SetAll writes every entry or none.
	SetAll(ctx context.Context, entries ...Entry) error
	// SetKeepTTL writes the value and keeps the key's expiry.
	SetKeepTTL(ctx context.Context, key string, value []byte) error
	// SetNX writes the value only if the key does not exist, and reports
	// whether it did.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	// CompareAndDelete deletes the key only while it holds expected, and
	// reports whether it did: the release of a lock taken with SetNX.
	CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error)
	// Del deletes the keys, strings or sets.
	Del(ctx context.Context, keys ...string) error
	// Exists reports whether the key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// Expire sets the key's TTL; false when the key does not exist.
	Expire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// ScanPrefix returns every key that starts with prefix.
	ScanPrefix(ctx context.Context, prefix string) ([]string, error)

	// SAdd adds members to the set.
	SAdd(ctx context.Context, key string, members ...string) error
	// SRem removes members from the set.
	SRem(ctx context.Context, key string, members ...string) error
	// SMembers returns the set's members.
	SMembers(ctx context.Context, key string) ([]string, error)
	// SCard returns the set's size.
	SCard(ctx context.Context, key string) (int64, error)
	// SIsMember reports whether member is in the set.
	SIsMember(ctx context.Context, key string, member string) (bool, error)

	// Publish sends payload to the channel's subscribers.
	Publish(ctx context.Context, channel string, payload []byte) error
	// Subscribe returns once the subscription is active: a message published
	// after it returns is delivered.
	Subscribe(ctx context.Context, channels ...string) (Subscription, error)

	// Ping reports whether the store answers.
	Ping(ctx context.Context) error
}

// Entry is one key to write with SetAll; TTL 0 means no expiry.
type Entry struct {
	Key   string
	Value []byte
	TTL   time.Duration
}

// PipelinedPublisher is a Store that sends writes and a publish in one round
// trip. Each command stands alone, as if sent by itself: a refused write does
// not stop the others or the publish. A caller uses it when the Store has it,
// and the plain Set and Publish otherwise.
type PipelinedPublisher interface {
	SetEachAndPublish(ctx context.Context, entries []Entry, channel string, payload []byte) (setErrs []error, publishErr error)
}

// Message is one published payload.
type Message struct {
	Channel string
	Payload string
}

// Subscription delivers the messages of the channels it was opened on.
type Subscription interface {
	// Messages is closed when the subscription is closed. A lost Redis
	// connection does not close it: the client subscribes again on its own.
	Messages() <-chan Message
	Close() error
}
