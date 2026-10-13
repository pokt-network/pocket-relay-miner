//go:build test

package kv

import (
	"context"
	"errors"
	"sync/atomic"
)

// FailingSubscribes is a Store whose first n Subscribe calls fail, as Redis
// does while it is unreachable; every other call goes to the Store inside.
// Subscribed receives once per Subscribe that succeeded. Tests only.
type FailingSubscribes struct {
	Store
	n          atomic.Int64
	Subscribed chan struct{}
}

// NewFailingSubscribes wraps store so its first n Subscribe calls fail.
func NewFailingSubscribes(store Store, n int64) *FailingSubscribes {
	f := &FailingSubscribes{Store: store, Subscribed: make(chan struct{}, 16)}
	f.n.Store(n)
	return f
}

func (f *FailingSubscribes) Subscribe(ctx context.Context, channels ...string) (Subscription, error) {
	if f.n.Add(-1) >= 0 {
		return nil, errors.New("kv: subscribe refused (test)")
	}
	sub, err := f.Store.Subscribe(ctx, channels...)
	if err == nil {
		f.Subscribed <- struct{}{}
	}
	return sub, err
}
