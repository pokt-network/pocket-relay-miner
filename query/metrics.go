package query

import (
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/prometheus/client_golang/prometheus"
)

// Query metrics (reserved for future instrumentation)
var (
	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "queries_total",
			Help:      "Total number of chain queries",
		},
		[]string{"client", "method"},
	)

	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "query_errors_total",
			Help:      "Total number of query errors",
		},
		[]string{"client", "method"},
	)

	_ = observability.SharedFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "query_latency_seconds",
			Help:      "Query latency in seconds",
			Buckets:   observability.FineGrainedLatencyBuckets,
		},
		[]string{"client", "method"},
	)

	// Cache metrics (reserved for future instrumentation)
	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_hits_total",
			Help:      "Total number of cache hits",
		},
		[]string{"client", "cache_type"},
	)

	_ = observability.SharedFactory.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_misses_total",
			Help:      "Total number of cache misses",
		},
		[]string{"client", "cache_type"},
	)

	_ = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "cache_size",
			Help:      "Current cache size (number of entries)",
		},
		[]string{"client", "cache_type"},
	)

	// relayMiningDifficultyTargetBits exposes the onchain relay mining
	// difficulty per service as the leading-zero-bit count of the
	// target_hash. Gauge semantics: the value is valid indefinitely
	// until the next chain query for that service overwrites it.
	//
	// Each additional leading-zero bit halves the fraction of relays
	// that satisfy the target (bits=0 → every hash passes; bits=1 →
	// 1/2; bits=2 → 1/4; …). This is the single number that explains
	// why ha_relayer_relays_mined_total / relays_served_total varies
	// across services.
	//
	// Updated at cache-miss in GetServiceRelayDifficulty and
	// GetServiceRelayDifficultyAtHeight — i.e. whenever the chain is
	// actually consulted. Cache hits are the hot path and don't touch
	// the gauge; in steady state new session starts produce new
	// queries that refresh the value.
	//
	// Restart gap: after a fresh process start, the gauge has no
	// series until the first cache miss per service lands (typically
	// within one session boundary, ~1-2 min on mainnet). Alerts
	// should use `absent_over_time(...[5m])` rather than plain
	// `absent(...)` to avoid spurious pages during startup.
	//
	// Historical overwrites: GetServiceRelayDifficultyAtHeight can be
	// called for a session that started earlier than the latest one
	// on-chain; the last write wins. In practice session starts
	// advance monotonically so the value tracks the current session
	// boundary, not an older one.
	relayMiningDifficultyTargetBits = observability.SharedFactory.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "ha",
			Subsystem: "query",
			Name:      "relay_mining_difficulty_target_bits",
			Help:      "Leading zero bits of the onchain relay mining target_hash per service. Higher = harder (bits=N → ~1/2^N of relays mineable). Updated on cache-miss in the difficulty query path; absent for a service until its first miss lands (use absent_over_time for alerts).",
		},
		[]string{"service_id"},
	)
)

// recordRelayMiningDifficulty extracts the leading-zero-bit count from
// the onchain target_hash and sets the per-service gauge. Called from
// every difficulty query path after a successful chain fetch.
//
// Empty / zero-length / nil target_hash maps to 0 bits (base
// difficulty — every relay passes). This matches the Pocket chain
// invariant that an absent target means no mining floor, not an
// infinitely hard target.
func recordRelayMiningDifficulty(serviceID string, targetHash []byte) {
	relayMiningDifficultyTargetBits.
		WithLabelValues(serviceID).
		Set(float64(countLeadingZeroBits(targetHash)))
}

// countLeadingZeroBits returns the number of leading zero bits in the
// target_hash. Used to turn a 32-byte chain value into a single
// operator-friendly number where each +1 halves the fraction of
// relays that will satisfy the difficulty check.
//
// The outer loop walks bytes left-to-right; whole-zero bytes add 8
// and continue, but the first non-zero byte is decoded MSB→LSB and
// the function returns as soon as a set bit is found. Trailing bytes
// after that first non-zero byte cannot affect the leading-zero
// count, so the outer loop never needs another iteration once a
// non-zero byte is processed — the `return bits` after the inner
// loop covers that exit.
func countLeadingZeroBits(b []byte) int {
	bits := 0
	for _, by := range b {
		if by == 0 {
			bits += 8
			continue
		}
		for i := 7; i >= 0; i-- {
			if by&(1<<i) != 0 {
				return bits
			}
			bits++
		}
		return bits
	}
	return bits
}
