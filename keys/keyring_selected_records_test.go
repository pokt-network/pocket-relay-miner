package keys

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// key_names selects a few records out of a keyring that holds many, and until
// 2026-09-03 selecting them changed the POLICY as well as the set: a record the
// operator had damaged made the by-name branch return an error, which made the
// manager abandon every later reload while the file stayed broken. The List()
// branch had already been through that and calls it a bug in its own comments --
// one damaged record blocking the withdrawal of all the others.
//
// Owner decision, textual: "if they're requested by name and they don't work,
// they're ignored and the ones that do get loaded, noisily, and we go on. Like
// when we list". The noise is for the operator to find their own problem;
// nothing of ours breaks over it.

// TestASelectedCorruptRecordIsIgnoredAndTheRestAreLoaded is the policy itself.
func TestASelectedCorruptRecordIsIgnoredAndTheRestAreLoaded(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app", "app2")

	corruptRecord(t, recordsDir, "app2")

	keys, err := p.LoadKeys(context.Background())

	require.NoError(t, err,
		"a damaged record must NOT be returned as a failure: returning it is what "+
			"made the manager abandon the reload for as long as the file sat there")
	require.Len(t, keys, 1, "the record that still decodes is loaded")
	require.Equal(t, 1.0,
		testutil.ToFloat64(keyringUndecodableRecords.WithLabelValues(p.Kind())),
		"and the damage is visible: this gauge was not published at all when "+
			"key_names was set, so turning the option on used to blind the signal")
}

// TestEverySelectedRecordCorruptIsStillRefused pins the half that is NOT ignored.
// Nothing decoding is not a partial failure, it is a broken keyring -- a rotated
// passphrase produces exactly this with not one corrupt byte on disk -- and
// applying it would drop every supplier at once.
func TestEverySelectedRecordCorruptIsStillRefused(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app", "app2")

	corruptRecord(t, recordsDir, "app.")
	corruptRecord(t, recordsDir, "app2")

	keys, err := p.LoadKeys(context.Background())

	require.Error(t, err)
	require.Contains(t, err.Error(), "not one could be decoded")
	require.Contains(t, err.Error(), "was asked for 2 record file(s)",
		"with key_names the selection is what failed, so reporting the whole "+
			"directory's size would overstate the damage")
	require.Empty(t, keys)
}

// TestACorruptSelectedRecordDoesNotBlockAWithdrawal is the money case, and the
// mirror of TestACorruptRecordDoesNotBlockTheWithdrawalOfAnother for the by-name
// branch. Before this change the withdrawal below was never applied: the reload
// was abandoned on every tick, so the operator pulled a key and it went on
// signing for as long as the unrelated broken file was there.
func TestACorruptSelectedRecordDoesNotBlockAWithdrawal(t *testing.T) {
	parentDir, recordsDir := newOnDiskTestKeyring(t, map[string]string{
		"app":  testAppHex,
		"app2": secondAppHex,
		"app3": thirdAppHex,
	})
	p := newOnDiskProvider(t, parentDir, "app", "app2", "app3")
	m := NewMultiProviderKeyManager(
		logging.NewLoggerFromConfig(logging.DefaultConfig()),
		[]KeyProvider{p}, KeyManagerConfig{HotReloadEnabled: false})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start(context.Background()))
	require.Len(t, m.ListSuppliers(), 3, "precondition: all three selected keys held")

	corruptRecord(t, recordsDir, "app2")
	withdrawRecord(t, recordsDir, "app3")

	require.NoError(t, m.Reload(context.Background()))
	require.Len(t, m.ListSuppliers(), 1,
		"the withdrawal must be applied even though an unrelated selected record "+
			"is damaged: app3 was pulled and app2 cannot be decoded, so only app remains")
}
