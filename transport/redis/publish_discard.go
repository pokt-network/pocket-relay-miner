package redis

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/puzpuzpuz/xsync/v4"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// Giving up on a relay at WRITE time is a different population from refusing one
// at ENQUEUE time, which is what publish_reject.go counts. A rejected relay was
// malformed and a producer of ours built it wrong; a discarded relay was
// well-formed and the STORE would not take it. They are separated here rather
// than sharing a reason label because publishRejectedTotal's Help says these
// were refused "by which validation refused them", and no validation of ours
// refused these.
//
// Both, though, mean the same thing to an operator: the relay was served, signed
// and answered to a client, and it will never be billed.
//
// Discarding at all is a decision, taken 2026-09-12: a chunk that can never be
// written used to go back to the head of the queue forever, and because the
// dispatcher stops at the first failed chunk, one poisoned stream stopped every
// supplier's relays from draining. Losing the relays of the affected stream is
// strictly better than losing everyone's.
const (
	// discardReasonWrongType is a key that exists holding another type. It is
	// permanent by inspection: no retry converts a string into a stream.
	discardReasonWrongType = "stream_wrong_type"
	// discardReasonAttemptsExhausted is the net under the classifier above.
	// Recognising permanent failures by their text is fragile by construction,
	// so an unknown error that always fails would otherwise block the queue
	// forever -- the exact defect discarding exists to close.
	discardReasonAttemptsExhausted = "attempts_exhausted"
)

// maxPublishAttempts is how many DISPATCHES one entry may fail before it is
// given up on.
//
// It is not a count of round trips: go-redis retries a pool timeout MaxRetries+1
// = 4 times inside a single dispatch, each waiting up to the 6s pool timeout, so
// one attempt here already spans a long transient. Ten of them is far past any
// pool exhaustion that clears, and well short of forever -- which is what the
// alternative was.
const maxPublishAttempts = 10

var publishDiscardedTotal = observability.SharedFactory.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: metricsNamespace,
		Subsystem: metricsSubsystem,
		Name:      "publish_discarded_total",
		Help: "Mined relays the publisher gave up on writing, by why it gave up. " +
			"Every one of these was served and answered to a client, so a non-zero value is lost revenue",
	},
	[]string{"supplier_addr", "service_id", "reason"},
)

// poisonLogInterval is how often ONE stream may report that it is unwritable.
//
// The signal an operator needs is the STREAM, not the relay: a WRONGTYPE is
// state on the SERVER, so discarding the relay does not repair it and every
// later relay for that supplier fails the same way. One line per relay would be
// thousands per second under load; one line per stream per interval is the
// state change.
//
// At Error, which is ABOVE what the repository's logging policy gives a state
// change, and deliberately. The policy's Warn is for a transition an operator
// reads during an incident; this one is continuous loss of revenue that nothing
// repairs on its own -- discarding the relay does not clean the key -- so it
// needs attention and not merely visibility.
const poisonLogInterval = time.Minute

// poisonReport is what one stream has accumulated since it last logged.
//
// The count is the point. A rate-limited line that says a stream is unwritable
// and not HOW MUCH it swallowed lets an operator read the incident and miss that
// it cost five thousand relays; the counter has the number, but the log is what
// gets read first, so the line has to carry it too.
type poisonReport struct {
	last       time.Time
	sinceLast  int
	firstError string
}

var poisonLogLast = xsync.NewMap[string, *poisonReport]()
var poisonMu sync.Mutex

// noteDiscard records one discard for a stream and, when the interval has
// elapsed, returns how many it swallowed since the last report.
func noteDiscard(stream, cause string) (report bool, sinceLast int, firstError string) {
	poisonMu.Lock()
	defer poisonMu.Unlock()

	now := time.Now()
	r, loaded := poisonLogLast.Load(stream)
	if !loaded {
		r = &poisonReport{firstError: cause}
		poisonLogLast.Store(stream, r)
	}
	r.sinceLast++
	if r.firstError == "" {
		r.firstError = cause
	}
	if loaded && now.Sub(r.last) < poisonLogInterval {
		return false, 0, ""
	}
	sinceLast, firstError = r.sinceLast, r.firstError
	r.last, r.sinceLast, r.firstError = now, 0, ""
	return true, sinceLast, firstError
}

// recordDiscard counts the relay always, and reports the stream's state at most
// once per poisonLogInterval.
//
// The two reasons are investigated from opposite ends, and the line says so:
//
//   - stream_wrong_type: the relay is WELL FORMED and what is stale is the key,
//     so the useful fact is what the key actually holds. The error Redis returns
//     ("...a key holding the wrong kind of value") does not say which type it
//     is, so this asks. It is a round trip on the discard path only, which is
//     rare by definition and never on the common one.
//   - attempts_exhausted: nobody classified the error, so the useful facts are
//     the error itself, how big the entry was and how long it had been waiting.
func (p *BatchingPublisher) recordDiscard(ctx context.Context, q queued, cause error) {
	service := q.service
	if service == "" {
		service = "unknown"
	}
	publishDiscardedTotal.WithLabelValues(q.supplier, service, q.discardReason).Inc()

	detail := ""
	if cause != nil {
		detail = cause.Error()
	}
	report, sinceLast, firstError := noteDiscard(q.stream, detail)
	if !report {
		return
	}

	ev := p.logger.Error().
		Str("stream", q.stream).
		Str("supplier_addr", q.supplier).
		Str("service_id", service).
		Str("reason", q.discardReason).
		Int("relays_discarded_since_last_report", sinceLast).
		Str("first_error", firstError).
		Int("attempts", q.attempts)

	switch q.discardReason {
	case discardReasonWrongType:
		// "I could not look" must not read the same as "it has no type": an
		// unreported failure here would send an operator after the wrong thing.
		if t, err := p.client.Type(ctx, q.stream).Result(); err != nil {
			ev = ev.Str("actual_type", "unknown").Str("actual_type_error", err.Error())
		} else {
			ev = ev.Str("actual_type", t)
		}
	case discardReasonAttemptsExhausted:
		ev = ev.Int("relay_bytes", q.bytes)
		if !q.enqueuedAt.IsZero() {
			ev = ev.Dur("queued_for", time.Since(q.enqueuedAt))
		}
	}

	ev.Msg("giving up on writing relays to this stream; they were served and are now unbillable")
}
