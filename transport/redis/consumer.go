package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/transport"
)

var _ transport.MinedRelayConsumer = (*StreamsConsumer)(nil)

// blockInterval bounds how long an idle XREADGROUP waits before returning
// redis.Nil so the loop can look at ctx.Done(). It is NOT a polling interval:
// the read still returns the instant a relay arrives, so nothing about delivery
// latency depends on this number. It is the upper bound on how long Close()
// waits for the read loop to notice it should stop, so it is chosen small
// enough to sit well inside a Kubernetes termination grace period and large
// enough that an idle supplier's stream is not woken up for nothing.
// A var, not a const, only so a test can shrink it: nothing in production
// writes it.
var blockInterval = 5 * time.Second

// isStreamNotFoundError reports whether a Redis error indicates the stream (or
// its consumer group) does not exist yet. Redis surfaces this as a "no such
// key" error for missing keys and a "NOGROUP" error when the stream/group is
// absent for group operations (XREADGROUP, XPENDING, XCLAIM, etc.).
func isStreamNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no such key") || strings.Contains(msg, "NOGROUP")
}

// StreamsConsumer implements MinedRelayConsumer using Redis Streams with consumer groups.
// It provides exactly-once delivery semantics within the consumer group.
// Push architecture: the blocking read returns the instant data arrives.
//   - Each consumer holds 1 connection while parked on XREADGROUP
//   - Pool sizing: Allocate numSuppliers + 20 overhead for cache/pubsub
//   - A cancelled context does NOT interrupt a blocked call; the block
//     elapsing is what lets the loop see it (see blockInterval)
//   - Claims = money - we cannot afford ANY latency consuming relays.
type StreamsConsumer struct {
	logger     logging.Logger
	client     redis.UniversalClient
	config     transport.ConsumerConfig
	streamName string // Single stream per supplier: ha:relays:{supplierAddr}

	// Message channel
	msgCh chan transport.StreamMessage

	// Claiming rate limit (prevent excessive claiming when stream is idle)
	lastClaimTime time.Time
	claimMu       sync.Mutex

	// Lifecycle management
	mu       sync.RWMutex
	closed   bool
	cancelFn context.CancelFunc
	wg       sync.WaitGroup
}

// NewStreamsConsumer creates a new Redis Streams consumer.
// Push architecture: the blocking read delivers with no polling delay.
func NewStreamsConsumer(
	logger logging.Logger,
	client redis.UniversalClient,
	config transport.ConsumerConfig,
) (*StreamsConsumer, error) {
	if config.StreamPrefix == "" {
		return nil, fmt.Errorf("stream prefix is required")
	}
	if config.SupplierOperatorAddress == "" {
		return nil, fmt.Errorf("supplier operator address is required")
	}
	if config.ConsumerGroup == "" {
		return nil, fmt.Errorf("consumer group is required")
	}
	if config.ConsumerName == "" {
		return nil, fmt.Errorf("consumer name is required")
	}

	// Set defaults - VERY AGGRESSIVE for minimal latency
	// The blocking read returns instantly when data arrives, and holds the
	// connection while the stream is empty.
	// Claims = money, we cannot afford to be slow consuming relays
	if config.BatchSize <= 0 {
		config.BatchSize = 5000 // Large batch for throughput
	}
	// ClaimIdleTimeout: How long before we claim messages from crashed consumers
	if config.ClaimIdleTimeout <= 0 {
		config.ClaimIdleTimeout = 30000 // 30 seconds for claiming idle messages
	}

	// Channel buffer: 5000 messages by default to match batch size for smooth
	// pipelining; configurable for tests and constrained deployments.
	channelBufferSize := config.ChannelBufferSize
	if channelBufferSize <= 0 {
		channelBufferSize = 5000
	}

	// Single stream per supplier (simplified architecture)
	streamName := transport.SupplierStreamName(config.StreamPrefix, config.SupplierOperatorAddress)

	return &StreamsConsumer{
		logger:     logging.ForSupplierComponent(logger, logging.ComponentRedisConsumer, config.SupplierOperatorAddress),
		client:     client,
		config:     config,
		streamName: streamName,
		msgCh:      make(chan transport.StreamMessage, channelBufferSize),
	}, nil
}

// Consume returns a channel that yields mined relay messages.
func (c *StreamsConsumer) Consume(ctx context.Context) <-chan transport.StreamMessage {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		close(c.msgCh)
		return c.msgCh
	}

	// Create cancellable context
	ctx, c.cancelFn = context.WithCancel(ctx)
	c.mu.Unlock()

	// Two producer goroutines feed msgCh: the blocking read loop and the
	// reclaim ticker. The channel is closed by a third goroutine only after
	// BOTH producers have returned — closing it from either producer would
	// race the other's in-flight send (send on a closed channel panics and
	// takes the whole process down).
	producers := &sync.WaitGroup{}
	producers.Add(2)

	// Consumer group creation happens in connectFn with proper exponential
	// backoff retry via ReconnectionLoop.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer producers.Done()
		c.consumeLoop(ctx)
	}()

	// Reclaim on a timer of its own. It used to be triggered only by XReadGroup
	// returning redis.Nil, which the then-infinite block made impossible on a
	// real server, so it was unreachable: a relay
	// delivered to a consumer whose pod died before acking sat in that dead
	// consumer's PEL forever, and its supplier's whole claim silently vanished
	// (issue #25). The ticker runs regardless of what the read loop is doing;
	// the lastClaimTime guard inside claimPendingMessages' caller path keeps
	// the two triggers from stacking.
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer producers.Done()
		c.reclaimLoop(ctx)
	}()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		producers.Wait()
		close(c.msgCh)
	}()

	c.logger.Info().
		Str("stream", c.streamName).
		Str("consumer_group", c.config.ConsumerGroup).
		Msg("started consuming from supplier stream")

	return c.msgCh
}

// ensureConsumerGroup creates the consumer group for the single supplier stream if it doesn't exist.
func (c *StreamsConsumer) ensureConsumerGroup(ctx context.Context) error {
	// Try to create the consumer group (XGroupCreateMkStream creates stream if needed)
	err := c.client.XGroupCreateMkStream(ctx, c.streamName, c.config.ConsumerGroup, "0").Err()
	if err != nil {
		// Ignore "BUSYGROUP" error - group already exists
		if !strings.Contains(err.Error(), "BUSYGROUP") {
			return fmt.Errorf("failed to create consumer group for %s: %w", c.streamName, err)
		}
	}
	return nil
}

// reclaimLoop periodically recovers messages stuck in dead consumers' PELs.
// It runs as a producer on msgCh alongside consumeLoop; the channel close is
// owned by the coordinator in Consume, never by either producer.
func (c *StreamsConsumer) reclaimLoop(ctx context.Context) {
	interval := time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.claimMu.Lock()
			due := time.Since(c.lastClaimTime) >= interval
			if due {
				c.lastClaimTime = time.Now()
			}
			c.claimMu.Unlock()
			if due {
				c.claimPendingMessages(ctx)
			}
		}
	}
}

// consumeLoop is the main consumption loop with automatic reconnection.
// This wraps the message consumption with exponential backoff reconnection,
// matching the pattern in client/block_subscriber.go:145-194
func (c *StreamsConsumer) consumeLoop(ctx context.Context) {
	// Create reconnection loop
	reconnectLoop := NewReconnectionLoop(
		c.logger,
		"streams_consumer",
		// connectFn: Create consumer group proactively on connect/reconnect.
		// XGroupCreateMkStream creates both stream and group if they don't exist.
		// This ensures the group exists before we try to consume, avoiding NOGROUP errors.
		func(ctx context.Context) error {
			return c.ensureConsumerGroup(ctx)
		},
		// runFn: Consume messages until error or context cancellation
		func(ctx context.Context) error {
			return c.consumeMessagesUntilError(ctx)
		},
	)

	// Run until context cancellation (handles all reconnection logic)
	reconnectLoop.Run(ctx)
}

// consumeMessagesUntilError runs the message consumption loop until an error occurs.
// Returns error to trigger reconnection via the reconnection loop.
// The read blocks for blockInterval, then returns redis.Nil and loops.
//   - Returns INSTANTLY when data arrives (zero latency)
//   - Blocks for blockInterval when the stream is empty (no polling, and no
//     indefinite park either)
//   - A cancelled context is seen when that block elapses, NOT when it is
//     cancelled: go-redis sets no read deadline from it
//
// This is the most efficient approach - no polling, pure push.
func (c *StreamsConsumer) consumeMessagesUntilError(ctx context.Context) error {
	for {
		// Still push, not polling: the read returns the INSTANT data arrives, so
		// delivery latency is unchanged by the block interval. The interval only
		// bounds how long an IDLE read sits there.
		//
		// It is not zero, and cancelling the context is not what ends it.
		// Verified against go-redis v9.17.2: for a blocking command, cmdTimeout
		// returns 0 (redis.go:751); the context handed to the reader is
		// context.Background() unless ContextTimeoutEnabled is set, which
		// defaults to false (redis.go:764); and deadline(Background, 0) returns
		// noDeadline (internal/pool/conn.go). So BLOCK 0 sets NO read deadline
		// at all and the socket read blocks until Redis says something --
		// Close() would cancel the context, then hang in wg.Wait() until a
		// relay happened to arrive. On an idle supplier that is until
		// Kubernetes runs out of grace and SIGKILLs the pod.
		//
		// Each blocked call holds one connection from the pool.
		streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			Streams:  []string{c.streamName, ">"},
			Count:    c.config.BatchSize,
			Block:    blockInterval,
		}).Result()
		if err != nil {
			// The block elapsing is how a cancelled context becomes visible.
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if err == redis.Nil {
				// The block elapsed with no messages. Nothing to do: reclaim
				// runs on its own ticker (reclaimLoop), so this branch does not
				// need to trigger it -- gating reclaim on this read is exactly
				// the bug 8e3c66d fixed, and it could not fire at all while the
				// block was infinite.
				continue
			}

			// Handle NOGROUP error - recreate consumer group
			// This is a fallback safety net. Normally connectFn creates the group at startup.
			// This handles edge cases like external deletion of the consumer group.
			if strings.Contains(err.Error(), "NOGROUP") {
				c.logger.Debug().Err(err).Msg("consumer group missing (unexpected - recreating)")
				if groupErr := c.ensureConsumerGroup(ctx); groupErr != nil {
					// Failed to recreate consumer group - return error to trigger
					// reconnection loop with exponential backoff instead of tight loop
					c.logger.Warn().Err(groupErr).Msg("failed to recreate consumer group, triggering reconnection")
					return fmt.Errorf("failed to recreate consumer group: %w", groupErr)
				}
				// Successfully created consumer group, retry XREADGROUP
				continue
			}

			consumeErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "read_error").Inc()
			// Per-iteration under a Redis outage; the outage state is logged
			// by the reconnection loop this return feeds into.
			c.logger.Debug().Err(err).Msg("error reading from stream")
			return err
		}

		// Process messages (single stream, so streams[0])
		if len(streams) == 0 {
			continue
		}

		for _, message := range streams[0].Messages {
			msg, parseErr := c.parseMessage(message, c.streamName)
			if parseErr != nil {
				deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
				// Warn, not Error: the message is ACKed and deleted (handled),
				// but a malformed message signals a producer bug or version
				// skew worth seeing without debug logging on.
				c.logger.Warn().
					Err(parseErr).
					Str(logging.FieldMessageID, message.ID).
					Msg("failed to parse message")
				// Acknowledge AND delete bad message to avoid redelivery and keep stream clean
				if err := c.client.XAckDel(ctx, c.streamName, c.config.ConsumerGroup, "DELREF", message.ID).Err(); err != nil {
					c.logger.Debug().Err(err).Str(logging.FieldMessageID, message.ID).Msg("failed to XAckDel bad message")
				}
				continue
			}

			// Log consume details for tracing
			c.logger.Debug().
				Str("stream_name", c.streamName).
				Str("session_id", msg.Message.SessionId).
				Str("supplier", msg.Message.SupplierOperatorAddress).
				Str("service", msg.Message.ServiceId).
				Str("message_id", message.ID).
				Msg("consumed relay from supplier stream")

			// Record end-to-end latency
			if msg.Message.PublishedAtUnixNano > 0 {
				latency := time.Since(msg.Message.PublishedAt()).Seconds()
				endToEndLatency.WithLabelValues(
					c.config.SupplierOperatorAddress,
					msg.Message.ServiceId,
				).Observe(latency)
			}

			consumedTotal.WithLabelValues(
				c.config.SupplierOperatorAddress,
				msg.Message.ServiceId,
			).Inc()

			// Send to channel (blocks if channel is full)
			select {
			case c.msgCh <- msg:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// claimIdleFromOtherConsumers returns one page of pending entries that belong
// to OTHER consumers and have been idle past ClaimIdleTimeout, reassigned to
// this consumer, plus the cursor to continue from ("0-0" when the scan is
// done).
//
// It replaces a plain XAUTOCLAIM, which filters on idle time ALONE and never
// on whether the owning consumer is alive — verified against a live Redis: a
// consumer running XAUTOCLAIM reclaims its OWN pending entries. This reclaim
// exists to rescue deliveries stranded in a DEAD pod's PEL, so re-claiming
// our own in-flight work is never the goal: under a backlog, anything sitting
// in the delivery buffer longer than the timeout got re-delivered to us as a
// duplicate, and the deeper the lag the more duplicates it produced. Dedup
// kept the accounting right; the wasted work compounded.
func (c *StreamsConsumer) claimIdleFromOtherConsumers(
	ctx context.Context,
	start string,
) (msgs []redis.XMessage, next string, err error) {
	minIdle := time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond
	if start == "0-0" {
		start = "-"
	}

	// No Idle filter here on purpose: the age check belongs to XCLAIM below,
	// which Redis applies authoritatively (entries younger than MinIdle are
	// simply not handed over). XPENDING is used only to learn WHO owns each
	// entry, which XAUTOCLAIM never reveals.
	pending, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.streamName,
		Group:  c.config.ConsumerGroup,
		Start:  start,
		End:    "+",
		Count:  50, // page size; claimPendingMessages loops until drained
	}).Result()
	if err != nil {
		return nil, "0-0", err
	}
	if len(pending) == 0 {
		return nil, "0-0", nil
	}

	ids := make([]string, 0, len(pending))
	for _, entry := range pending {
		if entry.Consumer == c.config.ConsumerName {
			continue // our own in-flight delivery, not a stranded one
		}
		ids = append(ids, entry.ID)
	}

	// Advance past the last entry EXAMINED, not the last one claimed: a
	// reclaimed entry stays in the PEL (owned by us now), so restarting from
	// the same point would return the same page forever. The successor of
	// "<ms>-<seq>" is "<ms>-<seq+1>"; computing it avoids the exclusive-range
	// syntax "(", which not every Redis implementation accepts.
	next = nextStreamID(pending[len(pending)-1].ID)
	if len(pending) < 50 {
		next = "0-0" // last page
	}

	if len(ids) == 0 {
		return nil, next, nil // this page was all ours; keep scanning
	}

	msgs, err = c.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   c.streamName,
		Group:    c.config.ConsumerGroup,
		Consumer: c.config.ConsumerName,
		MinIdle:  minIdle,
		Messages: ids,
	}).Result()
	if err != nil {
		return nil, "0-0", err
	}
	return msgs, next, nil
}

// nextStreamID returns the smallest stream ID greater than id, so a scan can
// continue without the exclusive-range syntax.
func nextStreamID(id string) string {
	ms, seq, found := strings.Cut(id, "-")
	if !found {
		return id
	}
	n, err := strconv.ParseUint(seq, 10, 64)
	if err != nil {
		return id
	}
	return ms + "-" + strconv.FormatUint(n+1, 10)
}

// claimPendingMessages recovers messages stranded in the PEL of a consumer
// that crashed without acknowledging them.
//
// It drains the WHOLE eligible PEL, not just the first page: each
// claimIdleFromOtherConsumers call examines at most one page of pending
// entries and returns the cursor to continue the scan from, and a dead
// consumer can leave thousands of deliveries behind (a full read batch plus
// the delivery channel buffer). Stopping after one page would recover them at
// one page per tick — far slower than the claim window this reclaim exists to
// beat.
func (c *StreamsConsumer) claimPendingMessages(ctx context.Context) {
	start := "0-0"
	totalClaimed := 0

	for {
		messages, next, err := c.claimIdleFromOtherConsumers(ctx, start)
		if err != nil {
			// Stream may not exist yet - skip
			if isStreamNotFoundError(err) {
				return
			}
			if ctx.Err() == nil {
				c.logger.Debug().Err(err).Msg("error claiming idle messages")
			}
			return
		}

		if len(messages) > 0 {
			totalClaimed += len(messages)
			claimedMessages.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(len(messages)))
		}

		// Process claimed messages. These are reclaims — mark them so downstream
		// workers know to run duplicate-detection before incrementing the
		// per-session counter.
		for _, message := range messages {
			msg, parseErr := c.parseMessage(message, c.streamName)
			if parseErr != nil {
				deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
				// Acknowledge AND delete bad message to keep stream clean
				_ = c.client.XAckDel(ctx, c.streamName, c.config.ConsumerGroup, "DELREF", message.ID)
				continue
			}
			msg.IsReclaim = true

			select {
			case c.msgCh <- msg:
			case <-ctx.Done():
				return
			}
		}

		// A returned cursor of "0-0" means the scan wrapped: the whole PEL has
		// been examined. The empty-string check is defensive.
		if next == "0-0" || next == "" {
			break
		}
		start = next

		if ctx.Err() != nil {
			return
		}
	}

	if totalClaimed > 0 {
		c.logger.Debug().
			Int("count", totalClaimed).
			Str("stream", c.streamName).
			Msg("claimed idle messages")
	}
}

// parseMessage deserializes a Redis Stream message into a StreamMessage.
// The streamName parameter is required for acknowledgment in multi-stream consumption.
//
// Memory optimization: Uses protobuf binary deserialization instead of JSON to eliminate
// JSON decoder memory overhead (literalStore accumulation). With 1000 suppliers consuming
// continuously, this reduces memory usage by ~67% (1.4GB → ~460MB) and improves throughput.
func (c *StreamsConsumer) parseMessage(message redis.XMessage, streamName string) (transport.StreamMessage, error) {
	data, ok := message.Values["data"]
	if !ok {
		return transport.StreamMessage{}, fmt.Errorf("message missing 'data' field")
	}

	dataStr, ok := data.(string)
	if !ok {
		return transport.StreamMessage{}, fmt.Errorf("message 'data' field is not a string")
	}

	// Deserialize from protobuf binary format into a pooled MinedRelayMessage
	// so we recycle the struct across relays instead of burning GC cycles on
	// a fresh heap allocation at 200+ RPS. The caller must Release the
	// message (see transport.ReleaseMinedRelayMessage) once processing is
	// complete — the consume loop in miner/supplier_manager.go owns that
	// responsibility.
	//
	// We return StreamMessage by value (not *StreamMessage) so the wrapper
	// stays on the caller's stack / goes directly into the channel buffer;
	// only the pooled Message pointer crosses the heap boundary.
	minedRelay := transport.AcquireMinedRelayMessage()
	if err := minedRelay.Unmarshal([]byte(dataStr)); err != nil {
		transport.ReleaseMinedRelayMessage(minedRelay)
		return transport.StreamMessage{}, fmt.Errorf("failed to unmarshal message: %w", err)
	}

	return transport.StreamMessage{
		ID:         message.ID,
		StreamName: streamName,
		Message:    minedRelay,
	}, nil
}

// AckMessage acknowledges a StreamMessage using its embedded stream name.
// This is the preferred method for acknowledging messages in multi-stream consumption.
func (c *StreamsConsumer) AckMessage(ctx context.Context, msg transport.StreamMessage) error {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	if msg.StreamName == "" {
		return fmt.Errorf("message missing stream name")
	}

	// Use XAckDel with DELREF to acknowledge AND delete the message from stream.
	// This prevents streams from growing unbounded - messages are removed after processing.
	// DELREF removes all references from all consumer groups (we only have one).
	err := c.client.XAckDel(ctx, msg.StreamName, c.config.ConsumerGroup, "DELREF", msg.ID).Err()
	if err != nil {
		return fmt.Errorf("failed to ack+delete message %s: %w", msg.ID, err)
	}

	ackedTotal.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
	return nil
}

// TrimStream removes entries older than the specified duration using MINID.
// NOTE: With XAckDel, messages are deleted on ack, so this is now a backup safety net
// for any orphaned messages that weren't properly acknowledged.
// Returns the number of entries trimmed.
func (c *StreamsConsumer) TrimStream(ctx context.Context, maxAge time.Duration) (int64, error) {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return 0, nil // Don't error on closed consumer - just skip trimming
	}
	c.mu.RUnlock()

	// Calculate MINID timestamp: current time - maxAge
	// Redis stream IDs are in format <ms>-<seq>, so we use <timestamp>-0
	minTimestamp := time.Now().Add(-maxAge).UnixMilli()
	minID := fmt.Sprintf("%d-0", minTimestamp)

	// Use XTRIM with MINID and ~ (approximate) for efficiency
	// ~ allows Redis to optimize by trimming to the nearest whole node
	trimmed, err := c.client.XTrimMinID(ctx, c.streamName, minID).Result()
	if err != nil {
		// Stream may not exist - not an error
		if isStreamNotFoundError(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to trim stream %s: %w", c.streamName, err)
	}

	if trimmed > 0 {
		c.logger.Info().
			Int64("trimmed_entries", trimmed).
			Str("min_id", minID).
			Dur("max_age", maxAge).
			Msg("trimmed old entries from stream")
	}

	return trimmed, nil
}

// Close gracefully shuts down the consumer.
func (c *StreamsConsumer) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil
	}

	c.closed = true

	// Cancel context to stop goroutines
	if c.cancelFn != nil {
		c.cancelFn()
	}

	// Wait for goroutines to finish
	c.wg.Wait()

	c.logger.Info().Msg("Redis Streams consumer closed")
	return nil
}
