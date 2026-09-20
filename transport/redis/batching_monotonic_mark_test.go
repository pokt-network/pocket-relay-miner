//go:build test

package redis

import (
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// hasMonotonic reports whether t carries a monotonic clock reading.
//
// Read from go1.26.5's own source, not inferred: Round(0) is the canonical way
// to strip the reading (src/time/time.go:46, and stripMono at :225-231), and ==
// compares wall, ext and loc (:131-136). So a value that differs from its own
// stripped copy is one that carried it.
func hasMonotonic(t time.Time) bool { return t != t.Round(0) }

// TestTheMarksAdmissionMeasuresFromKeepTheirMonotonicReading is item 388's
// positive control, and it is deliberately NOT the test the queue asked for.
//
// The queue asked for a test that PROVOKES a wall-clock jump. That test cannot
// be written: a Time's monotonic reading lives in the unexported ext field,
// setMono is private (time.go:233-234), and the one public operation that moves
// the wall -- Add -- moves BOTH readings by the same duration (time.go:40-41,
// :1179-1187), while the ones that touch the wall alone (In, Local, UTC, Round,
// Truncate, unmarshalling) do not move the monotonic reading, they DELETE it
// (:43-46). There is no public way to build "wall moved, monotonic still".
//
// What can be tested is what the jump exploited. Sub, After and Before use the
// monotonic readings alone when BOTH operands carry one, and fall back to the
// wall clock when either does not (time.go:48-51). The defect was precisely
// that: the mark travelled as an int64 of UnixNano and came back through
// time.Unix, without a reading, so every comparison measured the wall and a
// clock step moved the answer with Redis healthy.
//
// So this pins the property the fix rests on, at BOTH doors: the pinch in
// DispatcherHealthy compares lastSuccess against inFlightSince and then against
// now, and one mark losing the reading brings the defect back on its own.
func TestTheMarksAdmissionMeasuresFromKeepTheirMonotonicReading(t *testing.T) {
	// Control positive for the instrument: without it, "carries a reading" and
	// "the check cannot tell" print the same answer. These two lines are also
	// the defect and the fix, side by side.
	now := time.Now()
	require.True(t, hasMonotonic(now), "control: time.Now carries a monotonic reading")
	require.False(t, hasMonotonic(time.Unix(0, now.UnixNano())),
		"control: an instant rebuilt from an integer does not -- this is item 388 in one line")
	require.True(t, hasMonotonic(now.Add(time.Hour)), "control: Add keeps the reading, so a fake clock built on time.Now still discriminates")

	client := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { _ = client.Close() })
	p := NewBatchingPublisher(zerolog.Nop(), client, "ha:relays", time.Hour)
	// Stopped first: the marks below are made by hand, and the dispatcher's own
	// ticker must not race them.
	require.NoError(t, p.Close())

	require.True(t, hasMonotonic(p.now()),
		"a publisher tells the time with time.Now, so both ends of the subtraction can carry a reading")

	p.markSuccess()
	mark := p.lastSuccess.Load()
	require.NotNil(t, mark, "premise: markSuccess stored something")
	require.True(t, hasMonotonic(*mark),
		"the mark admission measures from lost its monotonic reading: every comparison against it now measures "+
			"the WALL clock, so a clock step alone closes admission with Redis answering (item 388)")

	at := p.now()
	p.inFlightSince.Store(&at)
	inFlight := p.inFlightSince.Load()
	require.True(t, hasMonotonic(*inFlight),
		"the in-flight mark lost its monotonic reading: the pinch compares it against lastSuccess and then "+
			"against now, so this door alone brings the defect back")
}
