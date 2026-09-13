//go:build test

package redis

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
)

// TestAFailedRoundGoesBackInArrivalOrder: when every write of a round fails, all
// of its chunks return to the queue in the order they were published, and the
// queue counts their bytes again.
func TestAFailedRoundGoesBackInArrivalOrder(t *testing.T) {
	client := testredis.Client(t)
	client.AddHook(&alwaysFails{err: errors.New("connection reset by peer")})
	p := NewBatchingPublisher(zerolog.Nop(), client, testredis.Prefix(t), time.Hour)
	t.Cleanup(func() { _ = p.Close() })
	p.workers = 2

	fillOneStream(t, p, 2*maxChunkCommands)
	before := p.QueuedBytes()

	p.dispatchAll(context.Background())

	require.Equal(t, before, p.QueuedBytes(), "every failed chunk went back")
	var got []string
	for {
		chunk := p.takeChunk()
		if len(chunk) == 0 {
			break
		}
		got = append(got, servicesOf(chunk)...)
	}
	require.Len(t, got, 2*maxChunkCommands)
	for i, s := range got {
		require.Equal(t, fmt.Sprintf("svc-%04d", i), s, "relay %d went back out of order", i)
	}
}
