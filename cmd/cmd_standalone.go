package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/internal/memlimit"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/standalone"
)

const flagStandaloneConfig = "config"

// StandaloneCmd returns the command that runs the relayer and the miner in one
// process.
func StandaloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "standalone",
		Short: "Run the relayer and the miner in one process",
		Long: `Run the relayer and the miner in one process, from one config file.

The common sections (pocket_node, keys, logging, metrics, pprof, and redis url
and namespace) are written once, at the top level; relayer: and miner: hold the
keys only that side reads, the same keys as in its own config file.

The miner starts first and the relayer once the miner is serving; on SIGINT or
SIGTERM the relayer drains first, then the miner stops. One observability
server serves both sides' metrics.

Example:
  pocket-relay-miner standalone --config /path/to/standalone.yaml
`,
		RunE: runStandalone,
	}
	cmd.Flags().String(flagStandaloneConfig, "", "Path to standalone config YAML file (required)")
	cmd.Flags().Bool(flagStrictConfig, false, "Refuse to start when the config carries keys this binary does not understand (default: warn and start)")
	cmd.Flags().String(flagRedisURL, "", "Redis connection URL for both sides (overrides config)")
	cmd.AddCommand(standaloneValidateCmd())
	return cmd
}

func standaloneValidateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "validate",
		Short: "Validate a standalone config without starting anything",
		Long: `Validate a standalone config against the same checks standalone runs at startup.

Exits 0 if the config would boot, non-zero with the first error otherwise.

Example:
  pocket-relay-miner standalone validate --config /path/to/standalone.yaml`,
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
			return nil
		},
	}
	c.Flags().String(flagStandaloneConfig, "", "Path to standalone config YAML file (required)")
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
	if cmd.Flags().Changed(flagRedisURL) {
		url, _ := cmd.Flags().GetString(flagRedisURL)
		cfg.Relayer.Redis.URL = url
		cfg.Miner.Redis.URL = url
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
			return fmt.Errorf("--strict-config: refusing to start, %d key(s) standalone does not understand (listed above)", len(unknown))
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

	minerSide := side{name: "miner", serve: func(ctx context.Context, hooks sideHooks) error {
		hooks.setReadiness = setReadiness
		return serveMiner(ctx, logger, cfg.Miner, hooks)
	}}
	relayerSide := side{name: "relayer", serve: func(ctx context.Context, hooks sideHooks) error {
		return serveRelayer(ctx, logger, cfg.Relayer, hooks)
	}}

	logger.Info().Msg("starting standalone: miner first, then relayer")
	return runSides(ctx, logger, minerSide, relayerSide, sideHooks{openKeys: sharedKeys}, sigCh)
}
