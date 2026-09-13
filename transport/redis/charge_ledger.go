package redis

import (
	"sync"
	"time"
)

// Charge is the served cost of one (session, supplier) pair that has not been
// written to the pair's consumed counter yet.
type Charge struct {
	// Key is the pair's consumed counter, built by the relay meter.
	Key string
	// Supplier names the stream this charge prefers to travel with: a charge is
	// written in the same MULTI as XADDs of its supplier whenever one has room.
	Supplier string
	Amount   int64
	// TTL is set on the counter with EXPIRE NX in the same MULTI as the INCRBY.
	TTL time.Duration

	attempts int
}

// ChargeLedger holds, per consumed counter, what was served and not written yet.
// The relay meter adds to it when a relay is served and reads it to admit the
// next one; the batching publisher takes from it at dispatch and reports each
// write's outcome.
//
// An amount taken for a dispatch still counts as pending until its write has an
// outcome. Without that, admission would see the amount vanish between the take
// and the reply and let the pair over its budget for that window.
type ChargeLedger struct {
	mu      sync.Mutex
	pending map[string]*Charge
	writing map[string]int64
	written func(key string, amount, consumed int64)
}

// NewChargeLedger returns an empty ledger.
func NewChargeLedger() *ChargeLedger {
	return &ChargeLedger{
		pending: make(map[string]*Charge),
		writing: make(map[string]int64),
	}
}

// OnWritten sets what the ledger calls after an INCRBY succeeds, with the amount
// written and the counter's new value. The callback runs WITHOUT the ledger's
// lock and must call FinishWrite(key, amount); without a callback the ledger
// finishes the write itself.
func (l *ChargeLedger) OnWritten(fn func(key string, amount, consumed int64)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.written = fn
}

// Add records a served amount for the pair.
func (l *ChargeLedger) Add(key, supplier string, amount int64, ttl time.Duration) {
	if amount <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.pending[key]; ok {
		c.Amount += amount
		return
	}
	l.pending[key] = &Charge{Key: key, Supplier: supplier, Amount: amount, TTL: ttl}
}

// Pending is what the pair has served and not yet seen written: queued plus
// taken by a dispatch still waiting for its outcome.
func (l *ChargeLedger) Pending(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	total := l.writing[key]
	if c, ok := l.pending[key]; ok {
		total += c.Amount
	}
	return total
}

// Drop forgets what the pair has queued. A write already taken by a dispatch
// still completes, and its INCRBY + EXPIRE NX recreates the counter with a TTL.
func (l *ChargeLedger) Drop(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pending, key)
}

// FinishWrite removes a written amount from what is being written.
func (l *ChargeLedger) FinishWrite(key string, amount int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishLocked(key, amount)
}

func (l *ChargeLedger) finishLocked(key string, amount int64) {
	left := l.writing[key] - amount
	if left <= 0 {
		delete(l.writing, key)
		return
	}
	l.writing[key] = left
}

// takeAll moves every queued charge into the writing set and returns them.
func (l *ChargeLedger) takeAll() []Charge {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) == 0 {
		return nil
	}
	out := make([]Charge, 0, len(l.pending))
	for key, c := range l.pending {
		out = append(out, *c)
		l.writing[key] += c.Amount
		delete(l.pending, key)
	}
	return out
}

// untake puts back a charge that was never sent, with its attempts unchanged.
func (l *ChargeLedger) untake(c Charge) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishLocked(c.Key, c.Amount)
	l.requeueLocked(c)
}

// retry puts back a charge whose INCRBY Redis refused, counting the attempt. It
// reports false, and keeps nothing, once the attempts are exhausted: a counter of
// the wrong type refuses every INCRBY, and retrying it forever would keep its
// amount pending for good.
func (l *ChargeLedger) retry(c Charge) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.finishLocked(c.Key, c.Amount)
	c.attempts++
	if c.attempts >= maxPublishAttempts {
		delete(l.pending, c.Key)
		return false
	}
	l.requeueLocked(c)
	return true
}

// forget drops a charge whose EXEC outcome is unknown. It is not put back: the
// EXEC may have run, and charging it again would bill the pair twice.
func (l *ChargeLedger) forget(c Charge) {
	l.FinishWrite(c.Key, c.Amount)
}

// commit reports a successful INCRBY.
func (l *ChargeLedger) commit(c Charge, consumed int64) {
	l.mu.Lock()
	fn := l.written
	l.mu.Unlock()
	if fn == nil {
		l.FinishWrite(c.Key, c.Amount)
		return
	}
	fn(c.Key, c.Amount, consumed)
}

func (l *ChargeLedger) requeueLocked(c Charge) {
	if cur, ok := l.pending[c.Key]; ok {
		cur.Amount += c.Amount
		if c.attempts > cur.attempts {
			cur.attempts = c.attempts
		}
		return
	}
	cp := c
	l.pending[c.Key] = &cp
}
