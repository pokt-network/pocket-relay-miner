//go:build test

package keys

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// validKeyHex is a well-formed 64-char (32-byte) hex private key used to seed a
// valid supplier keys file for the concurrency tests.
const validKeyHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestFileKeyProvider_WatchThenCloseNoPanic verifies that fsnotify events racing
// with Close() never panic (e.g. send on a closed changeCh). The watch loop is
// guarded by mu+closed precisely to make this safe.
func TestFileKeyProvider_WatchThenCloseNoPanic(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	tempDir := t.TempDir()

	provider, err := NewFileKeyProvider(logger, tempDir)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the watch loop.
	_ = provider.WatchForChanges(ctx)

	// Generate filesystem events concurrently while Close() happens, to
	// exercise the event-vs-Close race in the watch goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			path := filepath.Join(tempDir, "k.yaml")
			_ = os.WriteFile(path, []byte("operator_address: pokt1x\nprivate_key_hex: "+validKeyHex+"\n"), 0600)
			_ = os.Remove(path)
		}
	}()

	// Close concurrently with the event storm. Must not panic.
	time.Sleep(2 * time.Millisecond)
	require.NotPanics(t, func() {
		require.NoError(t, provider.Close())
	})

	wg.Wait()

	// Close is idempotent.
	require.NoError(t, provider.Close())
}

// TestSupplierKeysFileProvider_WatchThenCloseNoPanic verifies the same race
// safety for the supplier.yaml provider, which previously had an unguarded send.
func TestSupplierKeysFileProvider_WatchThenCloseNoPanic(t *testing.T) {
	logger := logging.NewLoggerFromConfig(logging.DefaultConfig())
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "supplier.yaml")

	require.NoError(t, os.WriteFile(filePath, []byte("keys:\n  - "+validKeyHex+"\n"), 0600))

	provider, err := NewSupplierKeysFileProvider(logger, filePath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_ = provider.WatchForChanges(ctx)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			// Rewrite the watched file to produce Write/Create events.
			_ = os.WriteFile(filePath, []byte("keys:\n  - "+validKeyHex+"\n"), 0600)
		}
	}()

	time.Sleep(2 * time.Millisecond)
	require.NotPanics(t, func() {
		require.NoError(t, provider.Close())
	})

	wg.Wait()

	require.NoError(t, provider.Close())
}
