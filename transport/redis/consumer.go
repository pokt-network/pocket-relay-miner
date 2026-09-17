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

// pendingPageSize bounds one XPENDING page during a reclaim scan.
//
// It is ONE constant on purpose. The page size and the "this was the last page"
// test must agree: a COUNT larger than the termination threshold loops forever,
// and a COUNT smaller than it stops the drain early, stranding pending entries
// until the next tick -- silent relay loss with the shape of issue #25. They
// used to be two independent literals.
const pendingPageSize = 50

// reapIdleMultiplier scales ClaimIdleTimeout into the silence a consumer record
// must show before it is deleted.
//
// The reclaim already rescues a dead pod's deliveries after ONE ClaimIdleTimeout,
// so by the time this threshold is reached the record is expected to be an empty
// shell. The multiple buys margin for a consumer that is merely slow rather than
// dead: deleting one that is about to act would destroy its pending entries, and
// XGROUP DELCONSUMER offers no conditional form.
const reapIdleMultiplier = 10

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

	// ownPendingAfter and ownPendingDone are the read loop's progress through
	// the entries pending under this consumer's name when it started (see
	// deliverOwnPending). Only the read loop's goroutine touches them.
	ownPendingAfter string
	ownPendingDone  bool

	// Message channel
	msgCh chan transport.StreamMessage

	// health, when set, pauses reading and reclaiming while Redis cannot take
	// writes: entries stay in the stream and in the PEL, unacked and undeleted.
	health *StoreHealth

	// pause, when set, holds reading and reclaiming while it is Paused, for a
	// condition of the process rather than of Redis.
	pause IngestionPause

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
	// (issue #25). The ticker runs regardless of what the read loop is doing,
	// and it is the only trigger.
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
//
// It sweeps as soon as it starts and then every quarter of the idle timeout.
// The first sweep used to come one full idle timeout after start, so a consumer
// that lived less than that -- a supplier claimed and released every ~32 s, as
// the L3 of df5441c saw on 2026-09-11 -- never swept at all, and entries released
// to it (parked idle under the "released" sentinel, or unowned) waited for
// whoever outlived the timeout. Sweeping more often takes nothing younger: the
// sweep itself only claims entries idle past ClaimIdleTimeout. Reaping dead
// consumers stays on the full timeout.
// SetStoreHealth pauses this consumer while health says Redis cannot take writes.
// Call it before Consume.
func (c *StreamsConsumer) SetStoreHealth(health *StoreHealth) {
	c.health = health
}

// IngestionPause holds a consumer's reads while Paused. PauseChanged returns a
// channel closed the next time Paused may have changed.
type IngestionPause interface {
	Paused() bool
	PauseChanged() <-chan struct{}
}

// SetIngestionPause holds this consumer's reads and reclaims while pause is
// Paused. Call it before Consume.
func (c *StreamsConsumer) SetIngestionPause(pause IngestionPause) {
	c.pause = pause
}

// operable reports whether the consumer may read: Redis can take writes and
// nothing holds ingestion.
func (c *StreamsConsumer) operable() bool {
	return c.health.Operable() && (c.pause == nil || !c.pause.Paused())
}

// waitOperable returns once Redis can take writes and nothing holds ingestion,
// or with ctx's error. Reading is not refused under maxmemory, but what the
// miner does with a read relay is a write, so reading while full only moves
// relays from the stream into a PEL they cannot leave.
func (c *StreamsConsumer) waitOperable(ctx context.Context) error {
	for {
		changed := c.health.Changed()
		var pauseChanged <-chan struct{}
		if c.pause != nil {
			pauseChanged = c.pause.PauseChanged()
		}
		if c.operable() {
			return nil
		}
		select {
		case <-changed:
		case <-pauseChanged:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *StreamsConsumer) reclaimLoop(ctx context.Context) {
	idle := time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond

	// The read loop creates the group too, on its own goroutine, and may not
	// have yet: a sweep against a missing group fails as "stream not found",
	// which claimPendingMessages skips in silence, so the first sweep would do
	// nothing. Creating it is idempotent.
	if err := c.ensureConsumerGroup(ctx); err != nil && ctx.Err() == nil {
		c.logger.Debug().Err(err).Msg("failed to ensure consumer group before the first reclaim sweep")
	}
	if c.operable() {
		c.claimPendingMessages(ctx)
	}

	sweep := time.NewTicker(idle / 4)
	defer sweep.Stop()
	reap := time.NewTicker(idle)
	defer reap.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			if c.operable() {
				c.claimPendingMessages(ctx)
			}
		case <-reap.C:
			if c.health.Operable() {
				c.reapDeadConsumers(ctx)
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
		// runFn: hand over what is already pending under this consumer's
		// name, once, then consume new messages until error or cancellation
		func(ctx context.Context) error {
			if err := c.deliverOwnPending(ctx); err != nil {
				return err
			}
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
		if err := c.waitOperable(ctx); err != nil {
			return err
		}
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
				// Parsed from the pool and never handed over.
				transport.ReleaseMinedRelayMessage(msg.Message)
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

	// Idle prunes the scan SERVER-side; it does not decide who gets the entry.
	// XCLAIM below still applies MinIdle authoritatively, so correctness does
	// not depend on this filter -- it only stops the scan from paging through
	// our own hot in-flight deliveries, which are young by definition and are
	// the bulk of the PEL under a backlog. XPENDING remains the only command
	// that reveals WHO owns an entry, which XAUTOCLAIM never does.
	//
	// Safe against skipping: the cursor advances past the last entry RETURNED,
	// so a young entry filtered out here is simply not examined this pass --
	// and it was not claimable anyway. claimPendingMessages restarts every
	// drain from "0-0", so nothing is permanently skipped.
	pending, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: c.streamName,
		Group:  c.config.ConsumerGroup,
		Idle:   minIdle,
		Start:  start,
		End:    "+",
		Count:  pendingPageSize,
	}).Result()
	if err != nil {
		if !isStreamNotFoundError(err) {
			reclaimErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "xpending").Inc()
		}
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
	if len(pending) < pendingPageSize {
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
		if !isStreamNotFoundError(err) {
			reclaimErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "xclaim").Inc()
		}
		return nil, "0-0", err
	}
	return msgs, next, nil
}

// reapDeadConsumers deletes consumer records that hold nothing and have gone
// silent, so a group's consumer registry does not grow by one entry per supplier
// per pod recreation forever.
//
// Why this is needed at all: the consumer name embeds the hostname and pid, so a
// recreated pod always registers a NEW name and Redis keeps the old record until
// something removes it. Until 2026-08-20 nothing did; the registry stayed small
// only because the stream key carried a TTL and expiring the key took the group
// with it. That TTL destroyed un-consumed relays along the way and has been
// removed, which is what makes an explicit reaper mandatory rather than tidy.
//
// The guard is deliberately narrow, because XGROUP DELCONSUMER DISCARDS the
// pending entries of the consumer it deletes -- running it on a consumer that
// still owns deliveries is exactly the relay loss this whole change exists to
// stop. Three conditions, all required:
//
//  1. not ourselves;
//  2. Pending == 0, so there is nothing to discard;
//  3. idle beyond reapIdleMultiplier * ClaimIdleTimeout, so a merely slow
//     consumer is not mistaken for a dead one.
//
// Between reading condition 2 and issuing the delete there is a window, and
// Redis has no conditional delete to close it. Rather than assert the window is
// unreachable, the return value of DELCONSUMER -- the number of pending entries
// it destroyed -- is recorded. It must always be zero; if it ever is not, the
// metric names the loss instead of hiding it.
func (c *StreamsConsumer) reapDeadConsumers(ctx context.Context) {
	claimIdle := time.Duration(c.config.ClaimIdleTimeout) * time.Millisecond
	reapIdle := claimIdle * reapIdleMultiplier

	consumers, err := c.client.XInfoConsumers(ctx, c.streamName, c.config.ConsumerGroup).Result()
	if err != nil {
		if isStreamNotFoundError(err) {
			return
		}
		if ctx.Err() == nil {
			reclaimErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "xinfoconsumers").Inc()
			c.logger.Debug().Err(err).Msg("error listing stream consumers")
		}
		return
	}

	for _, consumer := range consumers {
		if consumer.Name == c.config.ConsumerName {
			continue
		}
		// The sentinel is the one candidate the three conditions below cannot
		// make safe. They were written for the record of a DEAD POD: the name
		// embeds hostname and pid, so such a record is inert -- once it reads
		// Pending == 0 it can never gain another entry, and the window between
		// reading that and deleting is unreachable in practice.
		//
		// releasedConsumerName breaks that premise, because ReleaseMessage's
		// pre-8.8 fallback parks entries under it on every transient processing
		// failure and every shutdown drain. It reaches Pending == 0 whenever the
		// reclaim drains it and its idle then grows like any other record, so it
		// DOES become a candidate -- and a release landing between XINFO
		// CONSUMERS and XGROUP DELCONSUMER would be destroyed, which for a relay
		// already served means it is never billed.
		//
		// The cost of skipping it is one permanent consumer record per group,
		// which is what the sentinel is: it names handed-back work, and an
		// operator reading XINFO CONSUMERS wants to see it.
		if consumer.Name == releasedConsumerName {
			continue
		}
		if consumer.Pending != 0 {
			continue
		}
		if consumer.Idle < reapIdle {
			continue
		}

		destroyed, delErr := c.client.XGroupDelConsumer(
			ctx, c.streamName, c.config.ConsumerGroup, consumer.Name,
		).Result()
		if delErr != nil {
			if ctx.Err() == nil {
				reclaimErrorsTotal.WithLabelValues(c.config.SupplierOperatorAddress, "xgroupdelconsumer").Inc()
				c.logger.Debug().Err(delErr).
					Str("dead_consumer", consumer.Name).
					Msg("error reaping dead consumer")
			}
			continue
		}

		reapedConsumersTotal.WithLabelValues(c.config.SupplierOperatorAddress).Inc()

		if destroyed > 0 {
			// The window described above actually opened. These are acknowledged
			// relays that no longer exist anywhere: Error level, not Debug, because
			// it fires once per occurrence and it is money.
			reapDestroyedPendingTotal.WithLabelValues(c.config.SupplierOperatorAddress).
				Add(float64(destroyed))
			c.logger.Error().
				Str("dead_consumer", consumer.Name).
				Int64("destroyed_pending", destroyed).
				Msg("reaped a consumer that gained pending entries after being observed empty; those relays are lost")
			continue
		}

		c.logger.Info().
			Str("dead_consumer", consumer.Name).
			Dur("idle", consumer.Idle).
			Msg("reaped dead stream consumer")
	}
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
				// Parsed from the pool and never handed over.
				transport.ReleaseMinedRelayMessage(msg.Message)
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

// deliverOwnPending hands the read loop, once, every entry already pending under
// this consumer's name when it starts, before it reads anything new.
//
// The name is per process (miner.UniqueConsumerName), so a supplier this
// process releases and takes again gets a consumer with the SAME name, and what
// the previous one could not hand back on its way out is this one's. Nothing
// else reads it: ">" returns only new entries, and the reclaim skips an entry
// its own consumer owns as an in-flight delivery. It waited for the process to
// restart under another name.
//
// Once, not on every reconnection: past the first pass, what is pending under
// the name is what this consumer delivered itself -- in the buffer, or being
// processed. A pass cut short by an error resumes after the last entry it
// handed over, so none is handed over twice.
func (c *StreamsConsumer) deliverOwnPending(ctx context.Context) error {
	if c.ownPendingDone {
		return nil
	}
	if err := c.waitOperable(ctx); err != nil {
		return err
	}
	after, err := c.eachOwnPending(ctx, c.ownPendingAfter, func(msg transport.StreamMessage) error {
		select {
		case c.msgCh <- msg:
			return nil
		case <-ctx.Done():
			transport.ReleaseMinedRelayMessage(msg.Message)
			return ctx.Err()
		}
	})
	c.ownPendingAfter = after
	if err != nil {
		return err
	}
	c.ownPendingDone = true
	return nil
}

// EachOwnPending calls fn with every entry pending under this consumer's name,
// oldest first, parsed and marked a reclaim; fn owns the message. Each entry is
// visited once, whatever fn does with it. It is meant for a consumer whose
// producers have stopped (Stop): with nothing adding to the list, what it
// visits is all that is left there.
func (c *StreamsConsumer) EachOwnPending(ctx context.Context, fn func(transport.StreamMessage)) error {
	_, err := c.eachOwnPending(ctx, "0", func(msg transport.StreamMessage) error {
		fn(msg)
		return nil
	})
	return err
}

// eachOwnPending pages through the entries pending under this consumer's name
// with IDs after the given one ("" meaning from the start), calling fn with
// each one that parses; one no longer in the stream is acknowledged, and one
// that does not parse is acknowledged and deleted, as the read loop does. It stops at fn's first error and returns it, with the ID of
// the last entry finished -- the point to resume from.
func (c *StreamsConsumer) eachOwnPending(
	ctx context.Context,
	after string,
	fn func(transport.StreamMessage) error,
) (string, error) {
	if after == "" {
		after = "0"
	}
	for {
		// An ID instead of ">" reads this consumer's own pending list and hands
		// over nothing new. No BLOCK: go-redis sends one only for Block >= 0.
		streams, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    c.config.ConsumerGroup,
			Consumer: c.config.ConsumerName,
			Streams:  []string{c.streamName, after},
			Count:    pendingPageSize,
			Block:    -1,
		}).Result()
		if err == redis.Nil {
			return after, nil
		}
		if err != nil {
			return after, fmt.Errorf("failed to read the pending entries of consumer %s: %w", c.config.ConsumerName, err)
		}
		if len(streams) == 0 || len(streams[0].Messages) == 0 {
			return after, nil
		}
		for _, message := range streams[0].Messages {
			if len(message.Values) == 0 {
				// Deleted from the stream while still pending -- TrimStream's
				// XTRIM leaves the pending entry behind. Nothing to hand over,
				// and no producer's defect: the trim is where it went.
				if ackErr := c.client.XAck(ctx, c.streamName, c.config.ConsumerGroup, message.ID).Err(); ackErr != nil {
					c.logger.Debug().Err(ackErr).Str(logging.FieldMessageID, message.ID).Msg("failed to XAck a pending entry no longer in the stream")
				} else {
					c.logger.Debug().Str(logging.FieldMessageID, message.ID).Msg("dropped a pending entry no longer in the stream")
				}
				after = message.ID
				continue
			}
			msg, parseErr := c.parseMessage(message, c.streamName)
			if parseErr != nil {
				deserializationErrors.WithLabelValues(c.config.SupplierOperatorAddress).Inc()
				if delErr := c.client.XAckDel(ctx, c.streamName, c.config.ConsumerGroup, "DELREF", message.ID).Err(); delErr != nil {
					c.logger.Debug().Err(delErr).Str(logging.FieldMessageID, message.ID).Msg("failed to XAckDel bad message")
				}
				after = message.ID
				continue
			}
			msg.IsReclaim = true // the worker runs its duplicate check: it may have been processed
			if fnErr := fn(msg); fnErr != nil {
				return after, fnErr
			}
			after = message.ID
		}
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

// ReleaseMessage hands a delivered entry back so ANOTHER consumer can take it,
// without acknowledging it. It is the opposite of AckMessage: the entry stays in
// the stream.
//
// Why this exists: AckMessage is XAckDel with DELREF, which deletes the entry.
// A consumer that is going away has no business deleting work it did not do --
// doing so removes the entry from the pending list the reclaim reads, so nothing
// can rescue it.
//
// Two paths, both MEASURED against real servers on 2026-09-01:
//
//   - XNACK SILENT, from Redis 8.8.0. Marks the entry unowned and sets its
//     delivery time to 0, so it is claimable immediately regardless of any
//     min-idle. SILENT is the mode Redis documents for a consumer that is
//     shutting down: the delivery "did not count".
//
//   - Older servers (the deployed cluster is 8.4.6) have no XNACK, so the entry
//     is claimed to a SENTINEL consumer with a high IDLE: idle enough for the
//     next reclaim to take it, and owned by nobody real so the reclaim's
//     skip-my-own-deliveries filter does not swallow it. This goes through a raw
//     Do because XClaimArgs does not expose IDLE in any go-redis version.
//
//     It used to claim the entry back to ITSELF, which measured as a no-op:
//     the owner never changed, the reclaim skipped it as its own in-flight
//     delivery, and on a single-miner fleet nothing else existed to rescue it.
//
// A NOPERM is NOT treated as "unsupported": an ACL that forbids XNACK is a
// deliberate operator decision, and degrading past it silently would hide it.
// releaseIdleMillis is what the pre-8.8 fallback writes as the entry's idle
// time: large enough that any reclaim's min-idle is already satisfied.
const releaseIdleMillis = 24 * 60 * 60 * 1000

// releasedConsumerName is the sentinel owner the pre-8.8 fallback parks a
// released entry under.
//
// It is not a real consumer and never reads: it exists so the entry has an owner
// that is nobody, which is the closest a server without XNACK can get to
// "unowned". The reclaim only skips entries owned by ITSELF, so parking them
// here makes them visible to every consumer including the one that let go.
//
// It shows up in XINFO CONSUMERS, deliberately: an operator counting pending
// entries per consumer can see how much work is in flight versus how much was
// handed back, and the name says which is which.
const releasedConsumerName = "released"

func (c *StreamsConsumer) ReleaseMessage(ctx context.Context, msg transport.StreamMessage) error {
	// Same guard AckMessage keeps: a closed consumer must not claim to have
	// handed anything over. The caller counts a failure here as abandoned, which
	// leaves the entry pending -- the safe outcome.
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return fmt.Errorf("consumer is closed")
	}
	c.mu.RUnlock()

	if msg.StreamName == "" {
		return fmt.Errorf("message missing stream name")
	}

	err := c.client.XNack(ctx, &redis.XNackArgs{
		Stream: msg.StreamName,
		Group:  c.config.ConsumerGroup,
		Mode:   redis.XNackModeSilent,
		IDs:    []string{msg.ID},
	}).Err()
	if err == nil {
		return nil
	}

	errText := err.Error()
	if strings.Contains(errText, "NOPERM") {
		return fmt.Errorf("XNACK is forbidden by ACL for this user, which is configuration rather than server version: %w", err)
	}
	if !strings.Contains(errText, "unknown command") {
		return fmt.Errorf("failed to release message %s: %w", msg.ID, err)
	}

	// Pre-8.8 fallback. Two things make it work, and the second is not obvious --
	// it was measured against a bare redis:8.4.6 on 2026-09-02, with
	// scripts/localonly/redisprobe/release-owner.sh:
	//
	//   - The IDLE: without it the entry stays young and a reclaim with a
	//     positive min-idle skips it.
	//   - The TARGET consumer. Claiming it back to c.config.ConsumerName leaves
	//     the entry owned by this very consumer, and the reclaim skips any entry
	//     whose owner is itself ("our own in-flight delivery, not a stranded
	//     one"). Measured: the owner stayed `consumer-A` and its own reclaim
	//     returned nothing, so "release" released it to nobody -- and on a
	//     single-miner fleet there is no other consumer to rescue it. Claiming to
	//     a sentinel makes the entry foreign to every real consumer, including
	//     this one, which is exactly what XNACK achieves on 8.8+ by leaving it
	//     unowned.
	claim := []any{
		"XCLAIM", msg.StreamName, c.config.ConsumerGroup, releasedConsumerName, 0,
		msg.ID, "IDLE", releaseIdleMillis, "JUSTID",
	}
	if claimErr := c.client.Do(ctx, claim...).Err(); claimErr != nil {
		return fmt.Errorf("failed to release message %s via XCLAIM fallback: %w", msg.ID, claimErr)
	}
	return nil
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

// StreamName is the one stream this consumer reads, and so the stream every
// message it delivers must be acknowledged on.
func (c *StreamsConsumer) StreamName() string { return c.streamName }

// ConsumerGroup is the group this consumer reads and acknowledges in.
func (c *StreamsConsumer) ConsumerGroup() string { return c.config.ConsumerGroup }

// LastGeneratedID returns the stream's last-generated-id (XINFO STREAM), the
// highest ID ever appended -- including entries not yet delivered to any
// consumer. A caller comparing it against what it has already handled uses it
// to tell "nothing more has ever been written" from "more exists, waiting to
// be delivered or reclaimed".
func (c *StreamsConsumer) LastGeneratedID(ctx context.Context) (string, error) {
	info, err := c.client.XInfoStream(ctx, c.streamName).Result()
	if err != nil {
		return "", fmt.Errorf("failed to get stream info for %s: %w", c.streamName, err)
	}
	return info.LastGeneratedID, nil
}

// RecordAcked counts n messages acknowledged outside AckMessage -- by a script
// that deletes them in the same call as other writes -- on the same series
// AckMessage counts on, so the metric keeps meaning "acknowledged" whichever
// path did it.
func (c *StreamsConsumer) RecordAcked(n int) {
	ackedTotal.WithLabelValues(c.config.SupplierOperatorAddress).Add(float64(n))
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

// Stop ends the consumer's producers -- the read loop and the reclaim -- and
// waits for them, without closing it: AckMessage and ReleaseMessage still work
// afterwards, which is what a teardown needs to hand back what is left under
// this consumer's name once nothing can add to it. Idempotent; Close calls it.
func (c *StreamsConsumer) Stop() {
	c.mu.RLock()
	cancel := c.cancelFn
	c.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
}

// Close gracefully shuts down the consumer.
func (c *StreamsConsumer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	c.Stop()

	c.logger.Info().Msg("Redis Streams consumer closed")
	return nil
}
