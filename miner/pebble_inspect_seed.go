//go:build test

package miner

import (
	"context"
	"time"
)

// SeedSessionForTest saves snap as one of the supplier's sessions, for tests
// outside this package that read the store through the Inspect methods.
func (b *PebbleStoreBackend) SeedSessionForTest(supplier string, snap *SessionSnapshot) error {
	_, err := b.sessionStore(supplier).CreateIfAbsent(context.Background(), snap)
	return err
}

// SeedDedupForTest marks relayHash as processed for the session.
func (b *PebbleStoreBackend) SeedDedupForTest(sessionID string, relayHash []byte) error {
	_, err := b.deduplicator().MarkProcessed(context.Background(), relayHash, sessionID)
	return err
}

// SeedLiveRootForTest checkpoints a live root of the supplier's tree.
func (b *PebbleStoreBackend) SeedLiveRootForTest(supplier, sessionID string, root []byte) error {
	return b.smstStore(supplier).set(context.Background(), smstLiveRoot, sessionID, root, time.Hour)
}
