//go:build test

package miner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestPublishSupplierUpdateRejectsUnknownActions pins the default branch.
//
// The switch used to have none: an unrecognised action wrote nothing to Redis,
// incremented supplier_registry_updates_total with that action as its label, and
// returned nil. The caller was told the registry had been updated and the metric
// backed the claim up, so the only way to notice was to read the keys. The
// unbounded label was the second half of the same hole -- any string a caller
// passed became a Prometheus series.
func TestPublishSupplierUpdateRejectsUnknownActions(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		client,
		SupplierRegistryConfig{},
	)

	const addr = "pokt1supplier_unknown_action"

	err := registry.PublishSupplierUpdate(ctx, SupplierUpdateAction("update"), addr, nil)
	require.Error(t, err,
		`"update" was a declared constant that nothing ever emitted; it must now be rejected `+
			`rather than silently share the add branch`)

	err = registry.PublishSupplierUpdate(ctx, SupplierUpdateAction("whatever"), addr, nil)
	require.Error(t, err, "an arbitrary action must not reach the metric's label set")

	// "draining" joins them: it wrote a per-supplier value nobody read and never
	// touched the index, so it had no observable effect on either reader. The
	// draining SEMANTICS did not disappear -- they live where they are actually
	// enforced, in the supplier cache entry removeSupplier writes.
	err = registry.PublishSupplierUpdate(ctx, SupplierUpdateAction("draining"), addr, nil)
	require.Error(t, err,
		"draining is not a membership change; the registry only tracks whether "+
			"this fleet handles the address")

	// The consequence, not just the return value: membership is untouched.
	members, redisErr := registry.ListSuppliers(ctx)
	require.NoError(t, redisErr)
	require.NotContains(t, members, addr,
		"a rejected action must leave no membership behind")
}

// TestPublishSupplierUpdateAcceptsTheRealActions guards the other direction: the
// new default branch must not reject the actions production emits.
func TestPublishSupplierUpdateAcceptsTheRealActions(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		client,
		SupplierRegistryConfig{},
	)

	const addr = "pokt1supplier_real_actions"
	for _, action := range []SupplierUpdateAction{
		SupplierUpdateActionAdd,
		SupplierUpdateActionRemove,
	} {
		require.NoError(t, registry.PublishSupplierUpdate(ctx, action, addr, nil),
			"action %q is emitted by production and must be accepted", action)
	}
}

// TestRegistryIndexMembershipIsTheContract pins the ONLY part of the registry
// that has readers: the index set. balance_monitor.go (ListSuppliers) and
// orphan_streams.go (KnownSupplierAddresses, via SMembers) both read it; nothing
// reads the per-supplier value. This is the safety net for removing that value:
// add must make an address a member, remove must take it out, and neither may
// disturb another address's membership.
func TestRegistryIndexMembershipIsTheContract(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	registry := NewSupplierRegistry(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		client,
		SupplierRegistryConfig{},
	)

	const a, b = "pokt1membership_a", "pokt1membership_b"

	require.NoError(t, registry.PublishSupplierUpdate(ctx, SupplierUpdateActionAdd, a, nil))
	require.NoError(t, registry.PublishSupplierUpdate(ctx, SupplierUpdateActionAdd, b, nil))

	members, err := registry.ListSuppliers(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{a, b}, members, "add must make an address a member")

	require.NoError(t, registry.PublishSupplierUpdate(ctx, SupplierUpdateActionRemove, a, nil))

	members, err = registry.ListSuppliers(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{b}, members,
		"remove must drop exactly one address and leave the others alone")
}
