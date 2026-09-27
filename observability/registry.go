package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// Component-specific registries to ensure clean metric separation.
// The miner should only expose miner metrics, and the relayer should only expose relayer metrics.
var (
	// MinerRegistry is the Prometheus registry for miner metrics.
	MinerRegistry = prometheus.NewRegistry()

	// RelayerRegistry is the Prometheus registry for relayer metrics.
	RelayerRegistry = prometheus.NewRegistry()

	// SharedRegistry is for metrics shared between components (cache, keys, etc.)
	SharedRegistry = prometheus.NewRegistry()

	// MinerFactory creates metrics registered to the miner registry.
	MinerFactory = promauto.With(MinerRegistry)

	// RelayerFactory creates metrics registered to the relayer registry.
	RelayerFactory = promauto.With(RelayerRegistry)

	// SharedFactory creates metrics registered to the shared registry.
	SharedFactory = promauto.With(SharedRegistry)
)

func init() {
	// logging.PanicRecoveriesTotal cannot register itself here without an
	// import cycle (observability imports logging). Register it into the
	// shared registry so the panic signal is actually served — it previously
	// lived in the prometheus default registry, which no binary exposes.
	SharedRegistry.MustRegister(logging.PanicRecoveriesTotal)
	// Same constraint: the async-writer drop counter lives in logging and
	// must be served, or log loss under load stays invisible to alerting.
	SharedRegistry.MustRegister(logging.LogMessagesDroppedTotal)
}

func init() {
	// Register standard Go metrics collectors to both registries
	MinerRegistry.MustRegister(collectors.NewGoCollector())
	MinerRegistry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	RelayerRegistry.MustRegister(collectors.NewGoCollector())
	RelayerRegistry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}
