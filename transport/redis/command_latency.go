package redis

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// commandLatency is how long a Redis command took FROM THIS PROCESS -- the pool
// wait, go-redis's own retries, the round trip and the execution, all of it.
//
// It exists because none of the pool's own series can answer "how close are we
// to losing work". WaitCount and WaitDurationNs are incremented only after a
// wait that ENDED IN A CONNECTION (internal/pool/pool.go returns early when the
// wait expires), so their mean is biased low and, worse, IMPROVES as the pool
// starts timing out. go-redis's ConnectionWaitTime callback has the identical
// early return. A hook cannot be fooled that way: its timer is its own and runs
// whether the command succeeds, fails or times out.
//
// It is also the only place that sees the RETRY cost. go-redis retries a pool
// timeout by itself (shouldRetry treats ErrPoolTimeout as retryable), and a
// hook sits OUTSIDE that loop -- the hook chain's base is baseClient.process,
// which is what calls processWithRetry -- so one observation here covers every
// attempt. A command approaching MaxRetries+1 times the pool timeout is a relay
// about to be thrown away.
var commandLatency = observability.SharedFactory.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "redis_command_seconds",
		Help: "Seconds a Redis command took from this process, including pool wait and go-redis retries " +
			"(recorded for failed commands too, unlike the pool's own wait series)",
		// Reaches past 24s on purpose: with the default MaxRetries of 3 and a
		// 6s pool timeout, the worst case a caller can observe is four pool
		// waits, and a histogram that tops out at 10s would put exactly the
		// interesting failures in +Inf where they have no value.
		Buckets: prometheus.ExponentialBuckets(0.0001, 3, 13),
	},
	[]string{"component", "command"},
)

// pipelineCommandLabel is the command label for a pipelined batch.
//
// One observation per batch, not one per command in it: a pipeline is ONE round
// trip and one pool acquisition, so attributing its duration to each command
// would multiply a single wait into N and make the histogram report work that
// never happened.
//
// The same label covers Pipeline and TxPipeline. That is not laziness: the
// public redis.Hook interface has one ProcessPipelineHook, and go-redis applies
// it to BOTH chains (rebuild() wraps s.current.pipeline and
// s.current.txPipeline with it), so a hook is handed []Cmder with no way to tell
// which it is.
const pipelineCommandLabel = "pipeline"

// CommandLatencyHook records commandLatency for every command a client issues.
//
// Register it with client.AddHook. It is a redis.Hook and nothing else, so it
// works on standalone, sentinel and cluster alike.
type CommandLatencyHook struct {
	// component labels every series this hook writes.
	component string
	// pipeline is resolved once: its label set never varies.
	pipeline prometheus.Observer
}

// NewCommandLatencyHook builds a hook whose series carry component, the binary
// the client belongs to ("relayer" or "miner").
func NewCommandLatencyHook(component string) *CommandLatencyHook {
	return &CommandLatencyHook{
		component: component,
		pipeline:  commandLatency.WithLabelValues(component, pipelineCommandLabel),
	}
}

// observeSingle resolves the per-command series. Command names are a bounded
// set (the Redis command table), which is why the name is a safe label and a
// key never would be.
func (h *CommandLatencyHook) observeSingle(name string, d time.Duration) {
	commandLatency.WithLabelValues(h.component, name).Observe(d.Seconds())
}

// DialHook passes through: dialing is not a command, and go-redis already has
// its own connection-create timing.
func (h *CommandLatencyHook) DialHook(next redis.DialHook) redis.DialHook { return next }

// ProcessHook times a single command.
func (h *CommandLatencyHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		h.observeSingle(cmd.Name(), time.Since(start))
		return err
	}
}

// ProcessPipelineHook times a pipelined batch.
//
// It MUST be implemented rather than returned as a pass-through. Leaving it
// free is the shortcut that hides exactly the traffic that matters next: the
// bulk publisher writes with TxPipelined, so a pass-through here would leave the
// histogram blind to the whole write path while a test full of single commands
// stayed green.
func (h *CommandLatencyHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		h.pipeline.Observe(time.Since(start).Seconds())
		return err
	}
}
