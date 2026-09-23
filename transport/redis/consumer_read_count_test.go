//go:build test

package redis

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/internal/testredis"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

// XREADGROUP bounds a read in entries, never bytes, and the consumer asked for
// BatchSize (1000 in Tilt) every time: with relays of a MiB one read brought a
// GiB. The COUNT is now derived from readBudgetBytes.

func TestReadCount_FirstReadAsksForOneEntry(t *testing.T) {
	c := &StreamsConsumer{config: transport.ConsumerConfig{BatchSize: 1000}}
	require.Equal(t, int64(1), c.readCount(), "nothing is known of the entries' size yet")
}

func TestReadCount_BigEntriesShrinkTheRead(t *testing.T) {
	c := &StreamsConsumer{config: transport.ConsumerConfig{BatchSize: 1000}}
	c.noteLargestEntry(1 << 20)
	require.Equal(t, int64(readBudgetBytes/(1<<20)), c.readCount(), "a read of 1 MiB entries fits the budget")

	c.channelBytes.Store(readBudgetBytes - 4<<20)
	require.Equal(t, int64(4), c.readCount(), "what waits in the channel comes out of the budget")

	c.channelBytes.Store(readBudgetBytes * 2)
	require.Equal(t, int64(1), c.readCount(), "a full channel sizes the read down to one entry, never to zero")
}

func TestReadCount_SmallEntriesReadTheWholeBatch(t *testing.T) {
	c := &StreamsConsumer{config: transport.ConsumerConfig{BatchSize: 1000}}
	c.noteLargestEntry(1200) // an eth_blockNumber relay
	require.Equal(t, int64(1000), c.readCount(), "small relays read BatchSize, as before")
}

func TestReadCount_OneBigEntryFadesAway(t *testing.T) {
	c := &StreamsConsumer{config: transport.ConsumerConfig{BatchSize: 1000}}
	c.noteLargestEntry(1 << 20)
	c.noteLargestEntry(0) // an empty read changes nothing
	require.Equal(t, int64(1<<20), c.largestEntry.Load())

	c.noteLargestEntry(1200)
	require.Equal(t, int64(1<<20-(1<<20)/16), c.largestEntry.Load(), "a smaller read lets it fall by a sixteenth")
	for i := 0; i < 200; i++ {
		c.noteLargestEntry(1200)
	}
	require.Equal(t, int64(1200), c.largestEntry.Load(), "it settles on the entries actually read")
	require.Equal(t, int64(1000), c.readCount())

	c.noteLargestEntry(2 << 20)
	require.Equal(t, int64(2<<20), c.largestEntry.Load(), "a bigger entry takes over at once")
}

// readCounts records the COUNT of every XREADGROUP for new entries (">").
type readCounts struct {
	mu     sync.Mutex
	counts []int64
}

func (h *readCounts) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (h *readCounts) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

func (h *readCounts) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		args := cmd.Args()
		if len(args) > 0 && strings.EqualFold(fmt.Sprint(args[0]), "xreadgroup") && fmt.Sprint(args[len(args)-1]) == ">" {
			for i := 0; i+1 < len(args); i++ {
				if strings.EqualFold(fmt.Sprint(args[i]), "count") {
					if n, ok := args[i+1].(int64); ok {
						h.mu.Lock()
						h.counts = append(h.counts, n)
						h.mu.Unlock()
					}
				}
			}
		}
		return next(ctx, cmd)
	}
}

func (h *readCounts) snapshot() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int64(nil), h.counts...)
}

// consumeSized runs a real consumer over n relays of relaySize bytes and
// returns the COUNT of every read it made until it had delivered them all.
func consumeSized(t *testing.T, supplier string, n, relaySize int) []int64 {
	t.Helper()
	client := testredis.Client(t)
	hook := &readCounts{}
	client.AddHook(hook)
	prefix := testredis.Prefix(t)
	stream := transport.SupplierStreamName(prefix, supplier)
	group := prefix + ":group"
	ctx := context.Background()
	require.NoError(t, client.XGroupCreateMkStream(ctx, stream, group, "0").Err())
	for i := 0; i < n; i++ {
		body := []byte(strings.Repeat(string(rune('a'+i)), relaySize))
		hash := sha256.Sum256(body)
		buf, err := (&transport.MinedRelayMessage{
			SessionId: "sess-read-count", SupplierOperatorAddress: supplier, RelayBytes: body, RelayHash: hash[:],
		}).Marshal()
		require.NoError(t, err)
		require.NoError(t, client.XAdd(ctx, &goredis.XAddArgs{Stream: stream, Values: map[string]any{"data": string(buf)}}).Err())
	}
	c, err := NewStreamsConsumer(zerolog.Nop(), client, transport.ConsumerConfig{
		StreamPrefix:            prefix,
		SupplierOperatorAddress: supplier,
		ConsumerGroup:           group,
		ConsumerName:            "reader",
		ClaimIdleTimeout:        int64(time.Hour / time.Millisecond),
		BatchSize:               1000,
		ChannelBufferSize:       int64(n),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	msgs := c.Consume(ctx)
	for i := 0; i < n; i++ {
		select {
		case msg := <-msgs:
			c.MarkDelivered(msg)
			transport.ReleaseMinedRelayMessage(msg.Message)
		case <-time.After(20 * time.Second):
			t.Fatalf("only %d of %d relays were delivered", i, n)
		}
	}
	require.Zero(t, c.channelBytes.Load(), "every delivered relay was taken out of the channel's count")
	return hook.snapshot()
}

func TestConsumer_BigRelaysAreReadInReadsSizedByBytes(t *testing.T) {
	counts := consumeSized(t, "pokt1read_count_big", 3, 1<<20)
	require.GreaterOrEqual(t, len(counts), 2, "premise: the first read took one entry, so a second one followed")
	require.Equal(t, int64(1), counts[0], "the first read asks for one entry, not BatchSize")
	require.LessOrEqual(t, counts[1], int64(readBudgetBytes/(1<<20)), "the second read is sized by the 1 MiB entries")
}

func TestConsumer_SmallRelaysReadTheWholeBatchFromTheSecondRead(t *testing.T) {
	counts := consumeSized(t, "pokt1read_count_small", 3, 64)
	require.GreaterOrEqual(t, len(counts), 2)
	require.Equal(t, int64(1), counts[0])
	require.Equal(t, int64(1000), counts[1], "small relays read BatchSize from the second read on")
}
