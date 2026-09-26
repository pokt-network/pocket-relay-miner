package redis

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
)

// A relay the publisher refuses is NOT an ordinary outcome: it means a producer
// of ours built a message that cannot be written, and the relay behind it was
// already served, signed and answered to a client. 1412 of them were lost in the
// 2026-09-11 load and nobody could say which check rejected them, because the
// only signal was one drop counter with a single generic reason and a Debug line.
//
// Two signals, and they do different jobs:
//
//   - the COUNTER below is unconditional and says WHICH check fired, so "what
//     conditions produce an invalid message" has an aggregate answer at any
//     moment. Its labels are a fixed four-value reason and the service, never
//     anything taken from the message.
//   - the LOG carries the detail a counter cannot, and is RATE LIMITED. The
//     repository's logging policy allows Warn for a per-message condition that
//     signals a producer defect, on the grounds that it is "bounded by the defect
//     existing" -- but here that does not hold: proxy.go builds a message with a
//     literal SessionEndHeight of 0 on one path (queue item 188), so if that path
//     is ever taken EVERY relay through it is invalid and an unbounded Warn would
//     be one line per relay at thousands per second. Bounded by time instead.
const (
	rejectReasonNilMessage    = "nil_message"
	rejectReasonNoSessionID   = "session_id_empty"
	rejectReasonBadEndHeight  = "session_end_height_invalid"
	rejectReasonPublisherShut = "publisher_closed"
)

var publishRejectedTotal = observability.SharedFactory.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "publish_rejected_total",
		Help: "Mined relays the publisher refused, by which validation refused them. " +
			"Every one of these was served and answered to a client, so a non-zero value is lost revenue, " +
			"not a rejected request",
	},
	[]string{"service_id", "reason"},
)

// rejectLogInterval is how often ONE (reason, service) pair may log its detail.
//
// A minute, not "once ever": once-only would make a defect reintroduced after a
// fix invisible, and the point of this line is to be seen while it is happening.
const rejectLogInterval = time.Minute

// rejectLogLast is "reason\x00service" -> when that pair last logged.
//
// xsync and not sync.Map: the repository forbids sync.Map and internal/conventions
// enforces it. The typed map is also the better tool here -- the value is a
// time.Time and sync.Map would hand it back as an any needing a type assertion,
// which is a branch that can silently do the wrong thing on a bad cast.
var rejectLogLast = xsync.NewMap[string, time.Time]()

// shouldLogReject reports whether this pair may log now, and records that it did.
func shouldLogReject(reason, service string) bool {
	key := reason + "\x00" + service
	now := time.Now()
	if last, loaded := rejectLogLast.Load(key); loaded && now.Sub(last) < rejectLogInterval {
		return false
	}
	rejectLogLast.Store(key, now)
	return true
}

// recordPublishReject counts the rejection always and logs its detail at most
// once per rejectLogInterval per (reason, service).
func recordPublishReject(logger logging.Logger, reason, service, detail string) {
	if service == "" {
		service = "unknown"
	}
	publishRejectedTotal.WithLabelValues(service, reason).Inc()
	if !shouldLogReject(reason, service) {
		return
	}
	// Warn and not Debug: this is a defect in one of our own producers, and the
	// relay it cost was already paid for by the backend. It must be visible
	// without turning on debug logging.
	logger.Warn().
		Str("reason", reason).
		Str("service_id", service).
		Str("detail", detail).
		Msg("refused to publish a mined relay: the relay was served and is now unbillable")
}
