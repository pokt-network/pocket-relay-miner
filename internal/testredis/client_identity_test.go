//go:build test

package testredis

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// pipelineCounter counts every pipeline that reaches the client's hooks.
type pipelineCounter struct{ n atomic.Int64 }

func (h *pipelineCounter) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (h *pipelineCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *pipelineCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.n.Add(1)
		return next(ctx, cmds)
	}
}

// A connection the pool opens mid-test sends no pipeline through the test's
// hooks. go-redis's CLIENT SETINFO on connect is a pipeline, and a hook that
// counts pipelines read it as a write: it made a dispatch test fail under load.
func TestClientDialSendsNoPipelineThroughHooks(t *testing.T) {
	client := Client(t)
	hook := &pipelineCounter{}
	client.AddHook(hook)
	ctx := context.Background()

	// Hold the pooled connection so the next command has to dial a new one.
	held := client.Conn()
	t.Cleanup(func() { _ = held.Close() })
	require.NoError(t, held.Ping(ctx).Err())
	before := client.PoolStats().TotalConns

	require.NoError(t, client.Ping(ctx).Err())

	require.Greater(t, client.PoolStats().TotalConns, before, "CONTROL: the second Ping must have dialed a new connection")
	require.Zero(t, hook.n.Load(), "a new connection sent a pipeline through the client's hooks")
}
