package logging

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/diode"
)

// Logger is a type alias for zerolog.Logger.
// We use zerolog directly instead of wrapping it with abstractions.
type Logger = zerolog.Logger

// Config contains logging configuration options.
type Config struct {
	// Level is the log level: "debug", "info", "warn", "error"
	// Default: "info"
	Level string `yaml:"level"`

	// Format is the log format: "json" or "text"
	// Default: "json"
	Format string `yaml:"format"`

	// Async enables asynchronous/non-blocking logging using a ring buffer.
	// Highly recommended for production (1000+ RPS) to avoid blocking on I/O.
	// Default: true
	Async bool `yaml:"async"`

	// AsyncBufferSize is the size of the async ring buffer (in bytes).
	// Larger buffer = more buffering capacity but more memory usage.
	// Default: 100000 (100KB)
	AsyncBufferSize int `yaml:"async_buffer_size"`

	// AsyncPollInterval is how often the async writer polls for messages (in milliseconds).
	// Shorter interval = lower latency but more CPU overhead.
	// Longer interval = less CPU overhead but batches more messages.
	// Default: 100 (100ms) - good balance for most use cases
	// Production high-throughput: 10-50ms
	AsyncPollInterval int `yaml:"async_poll_interval"`

	// Sampling enables probabilistic log sampling to reduce volume.
	// When enabled, only a fraction of logs at each level are written.
	// Useful for extremely high throughput scenarios.
	// Default: false
	Sampling bool `yaml:"sampling"`

	// SamplingInitial is the number of messages to log before sampling kicks in.
	// Default: 100
	SamplingInitial int `yaml:"sampling_initial"`

	// SamplingThereafter logs 1 in N messages after the initial count.
	// E.g., 10 means log 1 out of every 10 messages.
	// Default: 10
	SamplingThereafter int `yaml:"sampling_thereafter"`

	// EnableCaller adds caller information (file:line) to logs.
	// Useful for debugging but has performance overhead (~100ns per log).
	// Default: false (enable in development, disable in production)
	EnableCaller bool `yaml:"enable_caller"`
}

// DefaultConfig returns a Config with performance-optimized defaults.
func DefaultConfig() Config {
	return Config{
		Level:              "info",
		Format:             "json",
		Async:              true,   // Enable async by default for performance
		AsyncBufferSize:    100000, // 100KB buffer
		AsyncPollInterval:  100,    // 100ms poll interval - balances latency vs CPU
		Sampling:           false,  // Disabled by default, enable for extreme throughput
		SamplingInitial:    100,    // First 100 messages always logged
		SamplingThereafter: 10,     // Then 1 in 10
		EnableCaller:       false,  // Disabled by default for production performance
	}
}

// NewLoggerFromConfig creates a high-performance logger from configuration.
// Performance optimizations:
// - Async writing via diode (non-blocking ring buffer)
// - Optional sampling for extreme throughput
// - Zero-allocation field chaining API
func NewLoggerFromConfig(config Config) Logger {
	// Parse log level
	level := parseLevel(config.Level)

	// Determine output writer
	// Note: diode (async writer) handles batching, so no need for additional bufio
	var output io.Writer = os.Stderr

	// Configure output format
	if strings.ToLower(config.Format) == "text" {
		// ConsoleWriter for human-readable output (dev/debugging)
		output = zerolog.ConsoleWriter{
			Out:        os.Stderr,
			TimeFormat: "15:04:05",
			FormatLevel: func(i interface{}) string {
				var level string
				if ll, ok := i.(string); ok {
					switch ll {
					case "debug":
						level = "\033[35m" + "DBG" + "\033[0m" // Magenta
					case "info":
						level = "\033[32m" + "INF" + "\033[0m" // Green
					case "warn":
						level = "\033[33m" + "WRN" + "\033[0m" // Yellow
					case "error":
						level = "\033[31m" + "ERR" + "\033[0m" // Red
					case "fatal":
						level = "\033[31;1m" + "FTL" + "\033[0m" // Bold Red
					case "panic":
						level = "\033[31;1m" + "PNC" + "\033[0m" // Bold Red
					default:
						level = "???"
					}
				}
				return level
			},
		}
	}

	// Wrap with async diode writer for non-blocking I/O
	// This prevents logging from blocking the hot path (critical for 1000+ RPS)
	if config.Async {
		bufferSize := config.AsyncBufferSize
		if bufferSize <= 0 {
			bufferSize = 100000 // Default 100KB
		}

		pollInterval := config.AsyncPollInterval
		if pollInterval <= 0 {
			pollInterval = 100 // Default 100ms
		}

		// Diode writer: drops old messages when buffer full
		// pollInterval controls CPU usage - longer interval = less CPU overhead
		output = diode.NewWriter(output, bufferSize, time.Duration(pollInterval)*time.Millisecond, func(missed int) {
			// This callback is rarely hit in practice, only when buffer overflows
			// We can't use the logger here (recursion), so write directly to stderr
			if missed > 0 {
				_, _ = os.Stderr.WriteString("WARN: dropped log messages due to full buffer\n")
			}
		})
	}

	// Create base logger
	ctx := zerolog.New(output).Level(level).With().Timestamp()
	if config.EnableCaller {
		ctx = ctx.Caller()
	}
	logger := ctx.Logger()

	// Apply sampling if enabled (for extreme throughput scenarios)
	if config.Sampling {
		initial := config.SamplingInitial
		thereafter := config.SamplingThereafter
		if initial <= 0 {
			initial = 100
		}
		if thereafter <= 0 {
			thereafter = 10
		}

		// BasicSampler: log first N messages, then 1 in M
		sampler := &zerolog.BasicSampler{N: uint32(thereafter)}
		logger = logger.Sample(&zerolog.BurstSampler{
			Burst:       uint32(initial),
			NextSampler: sampler,
		})
	}

	return logger
}

// parseLevel returns the zerolog.Level for the given string. It returns InfoLevel
// if the string is not recognized.
func parseLevel(level string) zerolog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return zerolog.DebugLevel
	case "info":
		return zerolog.InfoLevel
	case "warn":
		return zerolog.WarnLevel
	case "error":
		return zerolog.ErrorLevel
	default:
		return zerolog.InfoLevel
	}
}

// WithComponent returns a child logger with the component field set.
func WithComponent(logger Logger, component string) Logger {
	return logger.With().Str(FieldComponent, component).Logger()
}

// WithSupplier returns a child logger with the supplier field set.
func WithSupplier(logger Logger, supplierAddr string) Logger {
	return logger.With().Str(FieldSupplier, supplierAddr).Logger()
}

// WithSession returns a child logger with the session_id field set.
func WithSession(logger Logger, sessionID string) Logger {
	return logger.With().Str(FieldSessionID, sessionID).Logger()
}

// WithService returns a child logger with the service_id field set.
func WithService(logger Logger, serviceID string) Logger {
	return logger.With().Str(FieldServiceID, serviceID).Logger()
}

// ForComponent returns a logger configured for a specific component.
// This is the preferred way to create component loggers.
func ForComponent(logger Logger, component string) Logger {
	return WithComponent(logger, component)
}

// ForSupplierComponent returns a logger configured for a supplier-specific component.
func ForSupplierComponent(logger Logger, component, supplierAddr string) Logger {
	return logger.With().
		Str(FieldComponent, component).
		Str(FieldSupplier, supplierAddr).
		Logger()
}

// ForServiceComponent returns a logger configured for a service-specific component.
func ForServiceComponent(logger Logger, component, serviceID string) Logger {
	return logger.With().
		Str(FieldComponent, component).
		Str(FieldServiceID, serviceID).
		Logger()
}

// ForSessionOperation returns a logger configured for session-specific operations.
func ForSessionOperation(logger Logger, sessionID string) Logger {
	return WithSession(logger, sessionID)
}

// WithMinerID returns a logger with the miner_id field set.
// This should be called early in miner startup to set the context for all logs.
func WithMinerID(logger Logger, minerID string) Logger {
	return logger.With().Str(FieldMinerID, minerID).Logger()
}

// WithReplica returns a logger with the replica role field set.
// Use ReplicaLeader or ReplicaStandby constants.
func WithReplica(logger Logger, role string) Logger {
	return logger.With().Str(FieldReplica, role).Logger()
}

// ForMiner returns a logger configured with miner_id and replica role.
// This is the preferred way to create the top-level miner logger.
func ForMiner(logger Logger, minerID, replica string) Logger {
	return logger.With().
		Str(FieldMinerID, minerID).
		Str(FieldReplica, replica).
		Logger()
}

// ReplicaStatusProvider is an interface for components that can provide replica status dynamically.
type ReplicaStatusProvider interface {
	IsLeader() bool
}

// replicaHook is a zerolog hook that dynamically adds replica status field.
type replicaHook struct {
	provider ReplicaStatusProvider
	minerID  string
}

// Run adds the dynamic replica field to each log event.
func (h *replicaHook) Run(e *zerolog.Event, level zerolog.Level, msg string) {
	if h.provider == nil {
		return
	}

	replica := ReplicaStandby
	if h.provider.IsLeader() {
		replica = ReplicaLeader
	}

	e.Str(FieldReplica, replica).Str(FieldMinerID, h.minerID)
}

// ForMinerDynamic returns a logger with dynamic replica status.
// The replica field is evaluated at log time based on the provider's IsLeader() result.
// This allows the logger to automatically reflect leader election changes.
func ForMinerDynamic(logger Logger, minerID string, replicaProvider ReplicaStatusProvider) Logger {
	hook := &replicaHook{
		provider: replicaProvider,
		minerID:  minerID,
	}
	return logger.Hook(hook)
}
