//go:build test

package miner

import (
	"fmt"
	"os"
	"strings"
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

func TestUniqueConsumerName_DiffersFromAnotherProcessesName(t *testing.T) {
	// The property that matters, expressed without spawning a process: the
	// discriminator is what a second process would differ in.
	got := UniqueConsumerName("same")
	require.NotEqual(t, got, fmt.Sprintf("same-%s-%d", hostnameOrUnknown(), os.Getpid()+1),
		"the pid is part of the identity")
}
