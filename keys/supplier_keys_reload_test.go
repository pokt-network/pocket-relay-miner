//go:build test

package keys

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// replacedKeyHex is a second well-formed key so the replacement file differs
// from the original.
const replacedKeyHex = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// TestSupplierKeysFileProvider_AtomicReplaceTriggersReload pins hot reload
// against the way keys are actually rotated in production: write a temp file,
// then rename it over the target (vim, sed -i, kubectl-projected volumes all
// do this). A watch on the FILE dies silently when the inode is renamed away
// — fsnotify auto-removes it and nothing re-arms — so the provider must watch
// the parent directory instead. Without that, the relayer keeps signing with
// the old key set indefinitely and logs nothing.
func TestSupplierKeysFileProvider_AtomicReplaceTriggersReload(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "supplier.yaml")

	require.NoError(t, os.WriteFile(filePath, []byte("keys:\n  - "+validKeyHex+"\n"), 0o600))

	provider, err := NewSupplierKeysFileProvider(logger, filePath)
	require.NoError(t, err)
	defer func() { _ = provider.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := provider.WatchForChanges(ctx)

	// Atomic replace: write to a temp path, rename over the watched file.
	tmpPath := filepath.Join(tempDir, ".supplier.yaml.tmp")
	require.NoError(t, os.WriteFile(tmpPath, []byte("keys:\n  - "+replacedKeyHex+"\n"), 0o600))
	require.NoError(t, os.Rename(tmpPath, filePath))

	select {
	case <-ch:
		// Signal received: reload fires after an atomic replace.
	case <-time.After(3 * time.Second):
		t.Fatal("no change signal after atomic replace: the file watch died with the old inode")
	}

	// The provider must also read the NEW content afterwards.
	keys, err := provider.LoadKeys(ctx)
	require.NoError(t, err)
	require.Len(t, keys, 1)

	// A SECOND atomic replace must still fire: the watch survives the first
	// replacement (a file-scoped watch dies with the first renamed-away
	// inode even if it caught the initial event).
	require.NoError(t, os.WriteFile(tmpPath, []byte("keys:\n  - "+validKeyHex+"\n"), 0o600))
	require.NoError(t, os.Rename(tmpPath, filePath))

	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("no change signal after the SECOND atomic replace: the watch did not survive replacement")
	}
}
