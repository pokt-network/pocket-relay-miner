//go:build test

package testredis

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// namedCounter records every command and pipeline command that reaches it.
type namedCounter struct{ names []string }

func (c *namedCounter) DialHook(next redis.DialHook) redis.DialHook { return next }
func (c *namedCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.names = append(c.names, cmd.Name())
		return next(ctx, cmd)
	}
}

func (c *namedCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			c.names = append(c.names, cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// A connection initialized after a hook is added sends its handshake through
// that hook; ProductCommands keeps it out. The client is built the way the
// product builds one (identity on), and the only initialized connection is held
// so the next command must initialize a fresh one.
func TestProductCommandsHidesAConnectionHandshake(t *testing.T) {
	Client(t) // a real Redis, or the "start one with" failure
	opt, err := redis.ParseURL(URL())
	require.NoError(t, err)
	client := redis.NewClient(opt)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()

	held := client.Conn()
	t.Cleanup(func() { _ = held.Close() })
	require.NoError(t, held.Ping(ctx).Err())

	raw := &namedCounter{}
	wrapped := &namedCounter{}
	client.AddHook(raw)
	client.AddHook(ProductCommands(wrapped))

	require.NoError(t, client.Ping(ctx).Err())

	require.Contains(t, raw.names, "hello", "CONTROL: the new connection's handshake reaches an unwrapped hook")
	require.Equal(t, []string{"ping"}, wrapped.names, "the wrapped hook sees only the command the test sent")
}
