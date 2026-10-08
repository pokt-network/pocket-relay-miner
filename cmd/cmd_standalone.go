package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/internal/memlimit"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/leader"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/standalone"
	"github.com/pokt-network/pocket-relay-miner/standalone/inspect"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const flagStandaloneConfig = "config"

// StandaloneCmd returns the command that runs the relayer and the miner in one
// process.
func StandaloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "standalone",
		Short: "Run the relayer and the miner in one process, with no Redis (standalone mode)",
		Long: `Run the relayer and the miner in one process, from one config file, with
their state in an embedded store (storage.path) and no Redis: the standalone
mode. For replicas and failover on a shared Redis, run the relayer and miner
subcommands instead: the high-availability mode.

The common sections (pocket_node, keys, logging, metrics, pprof, storage) are
written once, at the top level; relayer: and miner: hold the keys only that
side reads, the same keys as in its own config file.

The miner starts first and the relayer once the miner is serving; on SIGINT or
SIGTERM the relayer drains first, then the miner stops. One observability
server serves both sides' metrics.

Example:
  pocket-relay-miner standalone --config /path/to/standalone.yaml
`,
		RunE: runStandalone,
		// A runtime failure (the node, the store) is the error line; the usage
		// text after it only buries it.
		SilenceUsage: true,
	}
	cmd.Flags().String(flagStandaloneConfig, "", "Path to standalone config YAML file (required)")
	cmd.Flags().Bool(flagStrictConfig, false, "Refuse to start when the config carries keys this binary does not understand (default: warn and start)")
	cmd.AddCommand(standaloneValidateCmd(), standaloneInspectCmd())
	return cmd
}

func standaloneValidateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a standalone config without starting anything",
		Long: `Validate a standalone config against the same checks standalone runs at startup.

Exits 0 if the config would boot, non-zero with the first error otherwise.

With --check-stake it also queries the chain for each configured supplier's
staked (service, transport) pairs and reports the ones the relayer side has no
backend for, as relayer validate --check-stake does.

Example:
  pocket-relay-miner standalone validate --config /path/to/standalone.yaml --check-stake`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadStandaloneConfig(cmd)
			if err != nil {
				return fmt.Errorf("config is INVALID: %w", err)
			}
			// Validating is this command's job, so an unknown key fails here.
			if unknown := cfg.Warnings(); len(unknown) > 0 {
				return fmt.Errorf("config is INVALID: %d key(s) standalone does not understand:\n  %s",
					len(unknown), strings.Join(unknown, "\n  "))
			}
			configPath, _ := cmd.Flags().GetString(flagStandaloneConfig)
			fmt.Printf("config OK: %s would start\n", configPath)

			checkStake, _ := cmd.Flags().GetBool(flagCheckStake)
			if !checkStake {
				return nil
			}
			nodeOverride, _ := cmd.Flags().GetString(flagNode)
			return runCheckStake(cmd.Context(), cfg.Relayer, nodeOverride)
		},
	}
	c.Flags().String(flagStandaloneConfig, "", "Path to standalone config YAML file (required)")
	c.Flags().Bool(flagCheckStake, false, "Cross-check on-chain stake against the relayer side's backends (queries the chain)")
	c.Flags().String(flagNode, "", "Override the gRPC query node URL (default: pocket_node.query_node_grpc_url from config)")
	_ = c.MarkFlagRequired(flagStandaloneConfig)
	return c
}

// loadStandaloneConfig loads the file and runs the checks each side's own
// subcommand runs at startup.
func loadStandaloneConfig(cmd *cobra.Command) (*standalone.Config, error) {
	configPath, _ := cmd.Flags().GetString(flagStandaloneConfig)
	if configPath == "" {
		return nil, fmt.Errorf("--config is required")
	}
	cfg, err := standalone.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if err := validateMinerConfig(cfg.Miner); err != nil {
		return nil, fmt.Errorf("miner: %w", err)
	}
	return cfg, nil
}

func runStandalone(cmd *cobra.Command, _ []string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("standalone panic: %v", r)
		}
	}()

	// Before any address is read: the relayer subcommand does the same, the
	// miner subcommand never needed it.
	initSDKConfig()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	cfg, err := loadStandaloneConfig(cmd)
	if err != nil {
		return err
	}

	logger := logging.NewLoggerFromConfig(cfg.Logging)
	// One process, one memory limit: the relayer's queues and the miner's trees
	// share it, and the miner's ingestion brake reads the heap the relayer also
	// fills. That is the difference from running the two subcommands.
	memLimit := memlimit.Apply(logger)
	logValidationQueueCapacity(logger, cfg.Relayer, memLimit)

	unknown := cfg.Warnings()
	for _, w := range unknown {
		logger.Warn().Msg(w)
	}
	if len(unknown) > 0 {
		if strict, _ := cmd.Flags().GetBool(flagStrictConfig); strict {
			return strictConfigError("standalone", unknown)
		}
	}

	// Registered before either side starts, so a signal during startup is kept
	// and handled in order once both are up, never by two handlers.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var setReadiness func(observability.ReadinessCheck)
	if cfg.Metrics.Enabled || cfg.PProf.Enabled {
		obsServer := observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: cfg.Metrics.Enabled,
			MetricsAddr:    cfg.Metrics.Addr,
			PprofEnabled:   cfg.PProf.Enabled,
			PprofAddr:      cfg.PProf.Addr,
			Registry:       standaloneGatherer(),
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }() //nolint:errcheck // Stop logs every shutdown failure at Error before returning the last one; this deferred caller has nobody to hand it to
		setReadiness = obsServer.SetReadinessCheck

		// One collector for the process: the ha_runtime_* families describe
		// the process, not a side.
		runtimeMetrics := observability.NewRuntimeMetricsCollector(
			logger,
			observability.DefaultRuntimeMetricsCollectorConfig(),
			observability.MinerFactory,
		)
		if err := runtimeMetrics.Start(ctx); err != nil {
			return fmt.Errorf("failed to start runtime metrics collector: %w", err)
		}
		defer runtimeMetrics.Stop()
	}

	// One key manager for both sides: one watch and one reload of the keys
	// file, and each side registers its own OnKeyChange. Closed after both
	// sides have stopped.
	keyManager, err := keys.OpenManager(ctx, logger,
		cfg.Miner.Keys.KeysFile, keyringSettings(cfg.Miner.Keys.Keyring), cfg.Miner.Keys.HotReloadEnabled)
	if err != nil {
		return err
	}
	defer func() { _ = keyManager.Close() }()
	sharedKeys := func(context.Context, logging.Logger) (*keys.MultiProviderKeyManager, func(), error) {
		return keyManager, func() {}, nil
	}

	// The embedded store, and what both sides keep in it: the caches, the
	// meter and the registries (kv), the relay queue (broker), and the miner's
	// sessions and dedup marks (backend). Closed after both sides stopped.
	db, err := pebblestore.Open(logger, pebblestore.Config{
		Path:         cfg.Storage.Path,
		SyncInterval: cfg.Storage.SyncInterval,
	})
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error().Err(closeErr).Msg("failed to close the embedded store")
		}
	}()
	keyBuilder := redistransport.NewKeyBuilder(cfg.Miner.Redis.Namespace)
	store := kv.NewPebble(logger, db, keyBuilder)
	defer func() { _ = store.Close() }()
	broker := pebblequeue.NewBroker(logger, db, store, keyBuilder.StreamPrefix())
	// The store's and the queue's metrics, read at scrape time, on the shared
	// registry standaloneGatherer serves. Unregistered before the store closes
	// (deferred after its close, so run before it); a scrape already running
	// then is answered by the collectors' own closed-store check.
	storeMetrics, queueMetrics := db.Collector(), broker.Collector()
	if err := observability.SharedRegistry.Register(storeMetrics); err != nil {
		return fmt.Errorf("failed to register the embedded store metrics: %w", err)
	}
	defer observability.SharedRegistry.Unregister(storeMetrics)
	if err := observability.SharedRegistry.Register(queueMetrics); err != nil {
		return fmt.Errorf("failed to register the relay queue metrics: %w", err)
	}
	defer observability.SharedRegistry.Unregister(queueMetrics)
	// The miner builds its backend once it has its config; the inspect server
	// reads the miner's state through the latest one.
	var builtBackend atomic.Pointer[miner.PebbleStoreBackend]
	minerBackend := func(config miner.SupplierManagerConfig) miner.StoreBackend {
		backend := miner.NewPebbleStoreBackend(logger, db, broker, config)
		builtBackend.Store(backend)
		return backend
	}
	if cfg.Inspect.Enabled {
		server := inspect.New(logger, cfg.Inspect.Addr, inspect.Sources{
			Miner: func() inspect.Miner {
				if backend := builtBackend.Load(); backend != nil {
					return backend
				}
				return nil
			},
			Queues: broker,
			KV:     store,
		})
		if err := server.Start(); err != nil {
			return err
		}
		// Deferred after the store's close, so it runs before it: a request
		// never reads a closed store.
		defer server.Stop()
	}
	// Each side's store: the embedded kv store, and a health gate over the
	// disk it lives on, so a filling disk stops new work before writes fail.
	openStore := func(ctx context.Context, logger logging.Logger, component string, gate redistransport.StoreGate) (sideStore, func(), error) {
		health := redistransport.NewDiskStoreHealth(logger, db.DiskUsage, component, gate)
		if err := health.Start(ctx); err != nil {
			return sideStore{}, nil, err
		}
		return sideStore{kv: store, health: health}, func() {}, nil
	}
	openQueue := func(_ context.Context, _ logging.Logger, d relayPublisherDeps) (relayPublisher, func(), error) {
		return broker.Publisher(d.config.Redis.BatchPublishInterval()), func() {}, nil
	}

	minerSide := side{name: "miner", serve: func(ctx context.Context, hooks sideHooks) error {
		hooks.setReadiness = setReadiness
		return serveMiner(ctx, logger, cfg.Miner, hooks)
	}}
	relayerSide := side{name: "relayer", serve: func(ctx context.Context, hooks sideHooks) error {
		return serveRelayer(ctx, logger, cfg.Relayer, hooks)
	}}

	logger.Info().Msg("starting standalone: miner first, then relayer")
	return runSides(ctx, logger, minerSide, relayerSide, sideHooks{
		openKeys:      sharedKeys,
		openStore:     openStore,
		openPublisher: openQueue,
		minerBackend:  minerBackend,
		// One process, no peers: it holds the leadership and every lease.
		newElector: leader.NewExclusiveGlobalLeaderElector,
	}, sigCh)
}
