//go:build test

package miner

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestTeardownCanFinishWorkAsksTheKeyManager pins the distinction between the
// two teardown paths, which used to be indistinguishable in the log an operator
// reads while a supplier is disappearing:
//
//   - unstake on-chain or a rebalance, key still held: there IS pending work
//     worth draining. The stream can be consumed, the tree built, and claim and
//     proof signed. poktroll also keeps the services active until
//     DeactivationHeight, so serving continues until the chain says otherwise.
//     This is a real drain.
//   - key removed by the operator: there is NOTHING to drain. Without the key no
//     relay response, claim or proof can be signed, so "draining, waiting for
//     pending work" describes work that cannot happen.
//
// The question is answered by asking the key manager, not by threading a reason
// from the caller, and that is the part worth a test: in distributed mode a key
// removal tears down through the claimer's release path, the same one a plain
// rebalance uses, so a threaded reason would be wrong in exactly the mode
// production runs.
func TestTeardownCanFinishWorkAsksTheKeyManager(t *testing.T) {
	const held, gone = "pokt1keystillhere", "pokt1keypulled"

	for _, tt := range []struct {
		name       string
		keyManager *fakeKeyManager
		supplier   string
		want       bool
	}{
		{
			name:       "key still held: a drain has something to drain",
			keyManager: &fakeKeyManager{addrs: []string{held}},
			supplier:   held,
			want:       true,
		},
		{
			name:       "key removed: nothing can be signed, so nothing to drain",
			keyManager: &fakeKeyManager{addrs: []string{held}},
			supplier:   gone,
			want:       false,
		},
		{
			name:       "no keys at all",
			keyManager: &fakeKeyManager{},
			supplier:   held,
			want:       false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &SupplierManager{
				logger:     logging.NewLoggerFromConfig(logging.DefaultConfig()),
				keyManager: tt.keyManager,
			}

			require.Equal(t, tt.want, m.teardownCanFinishWork(tt.supplier),
				"the teardown must describe what it can actually do, and the key "+
					"manager is the only local authority on that")
		})
	}
}

// TestTeardownCanFinishWorkWithoutAKeyManager keeps the nil case explicit: it
// answers true, which is the message this path had before the distinction
// existed. Only tests construct a SupplierManager without a key manager.
func TestTeardownCanFinishWorkWithoutAKeyManager(t *testing.T) {
	m := &SupplierManager{logger: logging.NewLoggerFromConfig(logging.DefaultConfig())}

	require.True(t, m.teardownCanFinishWork("pokt1whoever"),
		"a nil key manager must not turn every teardown into a key-removal report")
}
