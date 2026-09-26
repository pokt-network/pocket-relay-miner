package miner

import (
	"fmt"
	mathrand "math/rand/v2"
	"os"
	"sync"
)

// The identity of THIS process, used both as the leader-lock value and as the
// base of the Redis stream consumer name. It has to be two things at once, and
// the pair is what makes the shape non-obvious:
//
//   - STABLE within the process. Redis identifies a consumer by name and by
//     nothing else, so a second name built later in the same process would
//     strand the first one's pending entries under a name nothing answers to.
//     That is what sync.Once buys here -- it is CORRECTNESS, not caching. Note
//     that TestUniqueConsumerName_IsStableWithinAProcess does NOT hold it:
//     measured, it passes with the Once removed, because a readable hostname
//     makes the value deterministic anyway. The Once only carries weight on the
//     FALLBACK path, where the value is random, and
//     TestProcessIdentity_FallbackIsStableWithinAProcess is what pins that.
//   - UNREPEATABLE across processes. Two replicas sharing an identity share a
//     PEL, and share a leader-lock value: the renew script is "extend only if
//     the value is still mine", so with equal values one replica renews against
//     the other's key and never learns it lost the lease.
//
// os.Hostname() gives both in practice, and its error used to be discarded. The
// fallback is where the care goes: a CONSTANT would satisfy stability and
// destroy uniqueness -- in containers every replica is PID 1 in its own
// namespace, so "unknown-host-1" is the same string everywhere. This repo has
// written that down twice already, in newLockToken (cache/lock_release.go) and
// in the tx nonce base (tx/tx_client.go), both saying a fallback that collapses
// to a fixed value makes two replicas collide with certainty.
//
// math/rand/v2 and not crypto/rand, following tx_client.go: the property needed
// is that two processes differ, not unpredictability -- anyone who could act on
// a predicted identity already has the Redis access to do worse -- and it is
// seeded per process with no error to handle, so there is no second fallback to
// get right. crypto/rand.Read never returns an error, so its error branch would
// be unreachable code guarding the exact case this comment is about.
// hostnameFn is os.Hostname, indirected so a test can reach the fallback path.
// Read on the caller's goroutine (inside the Once, which runs synchronously for
// whoever wins it), never from one this package spawns.
var hostnameFn = os.Hostname

var (
	processIdentityOnce     sync.Once
	processIdentityValue    string
	processIdentityFellBack bool
)

// buildProcessIdentity is the decision, separated from os.Hostname so the
// fallback branch is reachable from a test: os.Hostname() cannot be made to
// fail in-process, and a test of the happy path would prove nothing about the
// branch this exists for.
func buildProcessIdentity(hostname func() (string, error)) (string, bool) {
	host, err := hostname()
	if err != nil || host == "" {
		// The prefix names the degradation. instanceID reaches the leader lock
		// value AND every log line of the miner process, so an operator reading
		// either one sees WHY the identity looks unusual, without having to know
		// to look for it.
		return fmt.Sprintf("unknown-host-%08x-%d", mathrand.Uint32(), os.Getpid()), true
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid()), false
}

// ProcessIdentity returns this process's identity, computed once.
func ProcessIdentity() string {
	processIdentityOnce.Do(func() {
		processIdentityValue, processIdentityFellBack = buildProcessIdentity(hostnameFn)
	})
	return processIdentityValue
}

// ProcessIdentityUsedFallback reports whether the identity had to be built
// without a hostname. It is a queryable fact rather than a second return value
// on purpose: the two consumers of the identity must not each be responsible
// for reporting the degradation, or one of them eventually will not -- and a
// discarded bool is invisible to errcheck, so nothing would catch it.
func ProcessIdentityUsedFallback() bool {
	ProcessIdentity()
	return processIdentityFellBack
}
