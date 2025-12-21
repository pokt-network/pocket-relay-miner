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
	// Default: "localhost:6060" (localhost only for security)
	Addr string `yaml:"addr,omitempty"`
}
