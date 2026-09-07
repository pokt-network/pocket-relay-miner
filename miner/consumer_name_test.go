//go:build test

package miner

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// The consumer name is how REDIS identifies this process inside the stream
// group, and Redis has no other notion of identity: two processes sharing a
// name share a PEL. That makes a fixed name in a shared ConfigMap — which the
// schema invites, calling the field "unique" while nothing enforces it — worse
// than a misconfiguration. A crashed replica's stranded deliveries then look
// like the surviving replica's own in-flight work, and the reclaim path skips
// them forever: the lost-claim bug (#25), reinstated by configuration, with
// nothing to warn about it.
//
// So the process discriminator is appended ALWAYS. What an operator sets is a
// readable prefix, not the identity.

func TestUniqueConsumerName_AppendsTheProcessDiscriminatorToAConfiguredName(t *testing.T) {
	got := UniqueConsumerName("shared-in-a-configmap")

	require.NotEqual(t, "shared-in-a-configmap", got,
		"a configured name must NOT be used verbatim: two replicas would share a PEL")
	require.True(t, strings.HasPrefix(got, "shared-in-a-configmap-"),
		"the configured name must survive as a readable prefix, got %q", got)
	require.True(t, strings.HasSuffix(got, fmt.Sprintf("-%d", os.Getpid())),
		"the name must end in this process's pid, got %q", got)
}

func TestUniqueConsumerName_EmptyGetsTheDefaultPrefix(t *testing.T) {
	got := UniqueConsumerName("")
	require.True(t, strings.HasPrefix(got, "miner-"),
		"an unset name keeps the historical miner- prefix, got %q", got)
	require.True(t, strings.HasSuffix(got, fmt.Sprintf("-%d", os.Getpid())), got)
}

func TestUniqueConsumerName_IsStableWithinAProcess(t *testing.T) {
	// Two consumers built in one process must agree, or a restart-free
	// reconfiguration would strand the first one's PEL entries under a name
	// nothing answers to.
	require.Equal(t, UniqueConsumerName("x"), UniqueConsumerName("x"))
}

// The old version of this test built its expected value as
// "same-<hostnameOrUnknown()>-<pid+1>" and asserted "the pid is part of the
// identity". That pinned the premise this change removes: in containers every
// replica is PID 1 in its own namespace, so the pid discriminates within a host
// and not between replicas -- and a green test asserting it carried more
// authority than the comment that said the same thing.
//
// The property that actually matters is that two processes get different
// identities WITHOUT relying on the pid, and it is only observable on the path
// where the hostname is unavailable: with a hostname, uniqueness comes from the
// host itself.
func TestProcessIdentity_TwoProcessesDifferEvenWithNoHostname(t *testing.T) {
	failing := func() (string, error) { return "", errors.New("no hostname here") }

	first, fellBack := buildProcessIdentity(failing)
	second, _ := buildProcessIdentity(failing)

	require.True(t, fellBack, "a failing hostname must be reported as a fallback")
	require.NotEqual(t, first, second,
		"two processes that both fail to read a hostname must still get different "+
			"identities; a constant fallback is what makes two replicas share a PEL "+
			"and renew each other's leader lease")
	require.True(t, strings.HasPrefix(first, "unknown-host-"),
		"the fallback must name itself, so an operator reading a log line or the "+
			"lock value sees WHY the identity looks unusual, got %q", first)
}

// An empty hostname is as unusable as an error, and the code treats them alike;
// asserting it keeps that from being an accident of how the condition is written.
func TestProcessIdentity_EmptyHostnameIsAFallbackToo(t *testing.T) {
	empty := func() (string, error) { return "", nil }
	got, fellBack := buildProcessIdentity(empty)
	require.True(t, fellBack, "an empty hostname must fall back, not produce a leading dash")
	require.True(t, strings.HasPrefix(got, "unknown-host-"), got)
}

// The sync.Once behind ProcessIdentity is correctness, not caching: a second
// consumer name built later in the same process would strand the first one's
// pending entries under a name nothing answers to.
//
// TestUniqueConsumerName_IsStableWithinAProcess does NOT prove that, and this
// was measured rather than assumed: with the Once removed it still passes,
// because a readable hostname makes the value deterministic on its own. The
// Once only matters where the value is RANDOM, so the fallback path is the only
// place the property is observable -- which is why this test forces it.
func TestProcessIdentity_FallbackIsStableWithinAProcess(t *testing.T) {
	// The Once is RESET to a fresh one rather than saved and restored: go vet's
	// copylocks rejects copying a sync.Once by value, and it is right to -- a
	// copied Once has its own done flag and would let the body run twice.
	// Resetting is enough here because the real hostnameFn is restored first,
	// so the next caller recomputes the genuine identity.
	origFn := hostnameFn
	t.Cleanup(func() {
		hostnameFn = origFn
		processIdentityOnce = sync.Once{}
		processIdentityValue, processIdentityFellBack = "", false
	})

	hostnameFn = func() (string, error) { return "", errors.New("no hostname here") }
	processIdentityOnce = sync.Once{}
	processIdentityValue, processIdentityFellBack = "", false

	first := ProcessIdentity()
	second := ProcessIdentity()

	require.True(t, ProcessIdentityUsedFallback(), "the injected failure must produce a fallback")
	require.Equal(t, first, second,
		"the identity must be computed ONCE: a per-call random fallback would give the "+
			"stream consumer a new name mid-process and strand the previous name's PEL")
}
