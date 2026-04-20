//go:build test

package miner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
)

// TestSupplierState_StatusRace reproduces the data race between
// consumeForSupplier reading state.Status (for the "processing relay
// during drain" debug log) and removeSupplier writing state.Status =
// SupplierStatusDraining under suppliersMu. The existing test suite
// does not hit this window, so the race detector does not flag it
// today, but the read/write pair is unsynchronized by inspection.
//
// Under -race, this test MUST NOT flag a DATA RACE once Status is
// promoted to atomic.Int32 and all call sites use LoadStatus /
// StoreStatus. Before the fix (plain SupplierStatus field with
// direct reads/writes), the same access pattern under -race flags.
func TestSupplierState_StatusRace(t *testing.T) {
	state := &SupplierState{
		OperatorAddr: "pokt1race",
	}
	state.StoreStatus(SupplierStatusActive)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	const workers = 4

	// Readers: mimic consumeForSupplier's per-relay Status check.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = state.LoadStatus() == SupplierStatusDraining
				}
			}
		}()
	}

	// Writers: mimic removeSupplier toggling drain state.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if idx%2 == 0 {
						state.StoreStatus(SupplierStatusDraining)
					} else {
						state.StoreStatus(SupplierStatusActive)
					}
				}
			}
		}(i)
	}

	// Let the race window stay open long enough for the race detector
	// to observe reads and writes if they are unsynchronized.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSupplierManager_OnKeyChangeCtxRace reproduces the data race
// between Start() assigning m.ctx under m.mu and onKeyChange reading
// m.ctx without a lock on the key-manager callback goroutine.
//
// The existing test suite does not hit this window because key
// manager callbacks are only wired in end-to-end tests with a real
// key manager. Under -race, this test MUST NOT flag a DATA RACE
// once onKeyChange captures m.ctx under m.mu.RLock() (mirroring the
// Close() pattern for m.closed).
func TestSupplierManager_OnKeyChangeCtxRace(t *testing.T) {
	mgr := &SupplierManager{
		logger:    logging.NewLoggerFromConfig(logging.DefaultConfig()),
		config:    SupplierManagerConfig{MinerID: "race-test"},
		suppliers: make(map[string]*SupplierState),
	}
	// Pre-seed a context so the first onKeyChange call does not see nil
	// before Start() runs. The race we reproduce is the concurrent
	// re-assignment inside Start(), not nil-deref.
	initialCtx, initialCancel := context.WithCancel(context.Background())
	t.Cleanup(initialCancel)
	mgr.ctx = initialCtx

	// Hook: run onKeyChange's ctx read path without needing a real
	// key manager or supplier query client. keyChangeReadCtx captures
	// m.ctx through the same synchronization it uses in production.
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writer: mimic Start() re-assigning m.ctx under m.mu.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				ctx, cancel := context.WithCancel(context.Background())
				mgr.mu.Lock()
				mgr.ctx = ctx
				mgr.cancelFn = cancel
				mgr.mu.Unlock()
			}
		}
	}()

	// Readers: mimic the key-manager firing onKeyChange from an
	// arbitrary goroutine. Each reader captures m.ctx via the same
	// path onKeyChange uses in production.
	const readers = 4
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = mgr.keyChangeReadCtx()
				}
			}
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
}
