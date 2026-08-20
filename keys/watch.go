package keys

import (
	"context"
	"sync"

	"github.com/fsnotify/fsnotify"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// watchFileEvents runs the fsnotify watch loop shared by the file-based key
// providers. It forwards a non-blocking signal on changeCh whenever a watched
// fsnotify event matching opMask is observed, and stops when ctx is cancelled
// or the watcher channels are closed.
//
// The send is guarded by mu + closed so a watcher event that races with
// Close() can never send on (or close) an already-closed changeCh.
func watchFileEvents(
	ctx context.Context,
	logger logging.Logger,
	watcher *fsnotify.Watcher,
	changeCh chan<- struct{},
	mu *sync.Mutex,
	closed *bool,
	opMask fsnotify.Op,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&opMask != 0 {
				// Non-blocking send with mutex protection to avoid sending to
				// (or after) a closed channel.
				mu.Lock()
				if !*closed {
					select {
					case changeCh <- struct{}{}:
					default:
					}
				}
				mu.Unlock()
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			logger.Warn().Err(err).Msg("file watcher error")
		}
	}
}
