package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
)

// sideHooks is what serveRelayer and serveMiner take from the process that runs
// them: the relayer and miner subcommands each run one side, standalone runs
// both, and these are the only places the two differ.
type sideHooks struct {
	// openKeys returns the supplier key manager and what to call when the side
	// is done with it. A side that owns its manager closes it there; a side that
	// shares one does nothing, and its owner closes it once both sides are done.
	openKeys func(ctx context.Context, logger logging.Logger) (*keys.MultiProviderKeyManager, func(), error)

	// started is called once the side is serving, and returns the channel whose
	// receive tells it to shut down.
	started func() <-chan os.Signal

	// setReadiness, when not nil, installs the side's readiness check on the
	// process's observability server.
	setReadiness func(observability.ReadinessCheck)

	// kv, when not nil, is the store the caches, the meter and the registries
	// keep their state in; nil means Redis.
	kv kv.Store

	// openPublisher builds the relayer's mined-relay publisher.
	openPublisher func(ctx context.Context, logger logging.Logger, d relayPublisherDeps) (relayPublisher, func(), error)

	// minerBackend, when not nil, keeps the miner's relay queue, sessions and
	// dedup marks; nil means Redis.
	minerBackend miner.StoreBackend
}

// openOwnKeys opens a key manager the side owns: closed when the side is done.
func openOwnKeys(cfg config.KeysConfig) func(context.Context, logging.Logger) (*keys.MultiProviderKeyManager, func(), error) {
	return func(ctx context.Context, logger logging.Logger) (*keys.MultiProviderKeyManager, func(), error) {
		keyManager, err := keys.OpenManager(ctx, logger, cfg.KeysFile, keyringSettings(cfg.Keyring), cfg.HotReloadEnabled)
		if err != nil {
			return nil, nil, err
		}
		return keyManager, func() { _ = keyManager.Close() }, nil
	}
}

// waitForSignal is the started hook of a process that runs one side: it stops
// on SIGINT or SIGTERM, registered once the side is serving, as before.
func waitForSignal() <-chan os.Signal {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return sigCh
}
