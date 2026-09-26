//go:build test

package testredis

import (
	"context"
	"strings"

	"github.com/redis/go-redis/v9"
)

// handshakeCommands are the commands go-redis sends to set up a connection
// (HELLO, AUTH, SELECT, READONLY, CLIENT SETNAME/SETINFO). None of them is
// issued by this repository's code.
var handshakeCommands = map[string]bool{
	"hello":    true,
	"auth":     true,
	"select":   true,
	"readonly": true,
	"client":   true,
}

func isHandshake(cmd redis.Cmder) bool {
	return handshakeCommands[strings.ToLower(cmd.Name())]
}

// ProductCommands wraps a hook so it never sees a connection's handshake.
//
// go-redis initializes a connection the first time the pool hands it out, and
// it sends that handshake through the client's hooks. Which connection a
// command gets -- one already initialized, or a fresh one the pool warmed up in
// the background -- depends on timing, so a hook that counts or fails commands
// sees the handshake only sometimes. Register such a hook through this wrapper:
// what it asserts about is this repository's commands.
func ProductCommands(h redis.Hook) redis.Hook { return productOnly{h} }

type productOnly struct{ h redis.Hook }

func (p productOnly) DialHook(next redis.DialHook) redis.DialHook { return p.h.DialHook(next) }

func (p productOnly) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	wrapped := p.h.ProcessHook(next)
	return func(ctx context.Context, cmd redis.Cmder) error {
		if isHandshake(cmd) {
			return next(ctx, cmd)
		}
		return wrapped(ctx, cmd)
	}
}

func (p productOnly) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	wrapped := p.h.ProcessPipelineHook(next)
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if !isHandshake(cmd) {
				return wrapped(ctx, cmds)
			}
		}
		return next(ctx, cmds)
	}
}
