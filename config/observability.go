package config

// MetricsConfig contains Prometheus metrics configuration.
// Shared between miner and relayer for metrics exposure.
type MetricsConfig struct {
	// Enabled enables the metrics server.
	Enabled bool `yaml:"enabled"`

	// Addr is the address to expose metrics on.
	// Default: ":9090" for relayer, ":9092" for miner
	Addr string `yaml:"addr"`
}

// PprofConfig contains pprof profiling configuration.
// Shared between miner and relayer for debugging and profiling.
type PprofConfig struct {
	// Enabled enables pprof profiling server.
	// Default: false (disabled for production safety)
	Enabled bool `yaml:"enabled,omitempty"`

	// Addr is the address for pprof server.
	// Default: DefaultPprofAddr.
	Addr string `yaml:"addr,omitempty"`
}

// DefaultPprofAddr is where pprof listens when no address is configured, in
// both binaries. Loopback, because pprof serves heap and goroutine dumps to
// anyone who can reach it. 127.0.0.1 and not "localhost": the name goes
// through the resolver and can come back as ::1, or not at all in a minimal
// container. Reaching it from outside the host or container takes an explicit
// addr (for example "0.0.0.0:6060").
const DefaultPprofAddr = "127.0.0.1:6060"
