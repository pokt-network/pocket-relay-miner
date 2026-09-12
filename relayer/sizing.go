package relayer

import "runtime"

// Worker and Redis pool sizing for the relayer, computed ONCE and read from
// both places that need it.
//
// This exists because the two numbers had drifted apart. The Redis pool came
// from a literal 50 in transport/redis (a formula written for the MINER, which
// opens a connection per supplier), while the workers came from
// runtime.NumCPU() * 8 in the relayer's startup. The relayer's Redis demand has
// nothing to do with how many suppliers exist: it is how many of its own
// workers can be inside a Redis call at once. Sizing one from the other by hand
// is how they stopped matching, the same shape as the GOMAXPROCS literal that
// stopped matching its CPU limit.

// masterWorkersPerProc is how many master-pool workers each schedulable
// processor gets. The work is a mix of CPU-bound ring-signature verification
// and I/O-bound Redis and backend calls, so it deliberately oversubscribes.
const masterWorkersPerProc = 8

// redisPoolMargin is the headroom added to the bounded Redis users. It covers
// the connections the relayer takes outside the subpools -- the supplier and
// cache readers on the request path, pub/sub bookkeeping, the health probes --
// none of which is per-supplier.
const redisPoolMargin = 20

// WorkerSizing is the relayer's concurrency budget: one master pool split into
// three subpools, and the Redis pool that the Redis-using subpools need.
type WorkerSizing struct {
	// Master is the master worker pool's capacity.
	Master int
	// Validation verifies ring signatures and, in optimistic mode, meters.
	Validation int
	// Publish writes mined relays to Redis.
	Publish int
	// Metrics records Prometheus observations. It does NOT touch Redis, which
	// is why it is absent from RedisPoolSize -- see that method.
	Metrics int
}

// ComputeWorkerSizing derives the whole budget from the number of schedulable
// processors.
//
// procs must come from runtime.GOMAXPROCS(0), NOT runtime.NumCPU(): NumCPU
// reports the machine's cores and ignores the container's CPU limit, so a pod
// limited to 2 cores on an 18-core node would build 144 workers for 2 cores.
// GOMAXPROCS(0) is what the runtime will actually schedule on -- automaxprocs
// derives it from the cgroup quota at startup.
func ComputeWorkerSizing(procs int) WorkerSizing {
	if procs < 1 {
		procs = 1
	}
	return SizingFromMaster(procs * masterWorkersPerProc)
}

// ComputeWorkerSizingForProcess is ComputeWorkerSizing for THIS process.
//
// It exists so the GOMAXPROCS decision is testable. Reading runtime.GOMAXPROCS(0)
// inline at the call site would put the one line that matters -- the choice of
// GOMAXPROCS over NumCPU -- inside package main's startup, where no test can
// reach it and where reverting it to NumCPU would go unnoticed on any machine
// whose CPU limit equals its core count, which is every developer laptop.
func ComputeWorkerSizingForProcess() WorkerSizing {
	return ComputeWorkerSizing(runtime.GOMAXPROCS(0))
}

// SizingFromMaster splits an already-decided master capacity.
//
// It is separate from ComputeWorkerSizing because the proxy is handed a pool it
// did not build and must derive the SAME split from its capacity. Two copies of
// the 70/20/10 split is how the split and the pool size would drift.
func SizingFromMaster(master int) WorkerSizing {
	if master < 1 {
		master = 1
	}
	s := WorkerSizing{
		Master:     master,
		Validation: int(float64(master) * 0.7), // CPU-intensive ring signatures
		Publish:    int(float64(master) * 0.2), // I/O-bound Redis writes
		Metrics:    int(float64(master) * 0.1), // low-priority observability
	}
	if s.Validation < 1 {
		s.Validation = 1
	}
	if s.Publish < 1 {
		s.Publish = 1
	}
	if s.Metrics < 1 {
		s.Metrics = 1
	}
	return s
}

// RedisPoolSize is how many Redis connections the relayer's BOUNDED users can
// need at once.
//
// Validation + Publish + margin. Metrics is deliberately NOT a term: the
// metrics subpool only records Prometheus observations in memory and issues no
// Redis command. That is asserted rather than asked to be remembered --
// internal/conventions has an import rule that fails if the metric recorder
// ever reaches for the Redis transport, and it names this method.
//
// THERE IS DELIBERATELY NO TERM FOR SUPPLIERS OR APPLICATIONS, and the reason
// has to be written down or the next reader will "complete" the formula with an
// invented multiplication:
//
//   - Opening a session meter costs 4 Redis commands and is keyed per (session,
//     supplier), so a session rotation arrives as a burst of them.
//   - How big that burst is depends on how many APPLICATIONS are relaying
//     through this relayer at that moment. That is external demand: we do not
//     choose it, we cannot read it at startup, and outside a localnet we do not
//     know it. A pool sized with an unknowable term is a guess wearing
//     arithmetic, and it would lend the whole formula a precision it does not
//     have.
//   - In eager validation mode that burst is not bounded by anything here
//     either: the meter runs inline in the HTTP handler (proxy.go,
//     CheckAndConsumeRelay with r.Context()), not inside a subpool, so its
//     concurrency is whatever the HTTP server admits.
//
// So the burst is not SIZED, it is OBSERVED and CAPPED: observed through the
// pool metrics and the per-command latency histogram, capped by admission
// control. This number covers the bounded, knowable users and nothing else.
func (s WorkerSizing) RedisPoolSize() int {
	return s.Validation + s.Publish + redisPoolMargin
}
