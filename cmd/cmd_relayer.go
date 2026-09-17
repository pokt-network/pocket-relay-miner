package cmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/alitto/pond/v2"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/spf13/cobra"

	"github.com/pokt-network/pocket-relay-miner/cache"
	// Aliased because runHARelayer binds a local variable named `config` to
	// the relayer configuration, which would shadow the package name.
	sharedconfig "github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/internal/memlimit"
	"github.com/pokt-network/pocket-relay-miner/keys"
	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/observability"
	"github.com/pokt-network/pocket-relay-miner/query"
	"github.com/pokt-network/pocket-relay-miner/relayer"
	"github.com/pokt-network/pocket-relay-miner/rings"
	"github.com/pokt-network/pocket-relay-miner/transport"
	redistransport "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

const (
	flagRelayerConfig = "config"

	// flagStrictConfig turns the unknown-key diagnostic from a warning into a
	// refusal to start. Off by default because a rolling deploy lands a new
	// binary beside an older ConfigMap as a matter of course; on for the
	// operator who would rather not serve at all than serve with a config the
	// binary partly ignores.
	flagStrictConfig = "strict-config"
	flagRedisURL     = "redis-url"

	// Pocket Network Bech32 address prefix
	// Reference: poktroll/app/app.go:49
	accountAddressPrefix = "pokt"
)

// initSDKConfig initializes the Cosmos SDK configuration with Pocket Network's Bech32 prefix.
// This is required for address validation to work correctly with "pokt" addresses.
func initSDKConfig() {
	config := sdk.GetConfig()

	// Check if already initialized (config may be sealed)
	if config.GetBech32AccountAddrPrefix() == accountAddressPrefix {
		return
	}

	// Set Bech32 prefixes for Pocket Network
	accountPubKeyPrefix := accountAddressPrefix + "pub"
	validatorAddressPrefix := accountAddressPrefix + "valoper"
	validatorPubKeyPrefix := accountAddressPrefix + "valoperpub"
	consNodeAddressPrefix := accountAddressPrefix + "valcons"
	consNodePubKeyPrefix := accountAddressPrefix + "valconspub"

	config.SetBech32PrefixForAccount(accountAddressPrefix, accountPubKeyPrefix)
	config.SetBech32PrefixForValidator(validatorAddressPrefix, validatorPubKeyPrefix)
	config.SetBech32PrefixForConsensusNode(consNodeAddressPrefix, consNodePubKeyPrefix)
	config.Seal()
}

// startRelayerCmd returns the command for starting the HA Relayer component.
func RelayerCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relayer",
		Short: "Start the HA Relayer (HTTP/WebSocket proxy)",
		Long: `Start the High-Availability Relayer component.

The HA Relayer handles incoming relay requests and forwards them to backend services.
It is stateless and can be scaled horizontally behind a load balancer.

Features:
- HTTP and WebSocket relay proxying
- Request validation and signing
- Health checking for backends
- Prometheus metrics at /metrics

Example:
  pocketd relayminer ha relayer --config /path/to/ha-relayer.yaml --redis-url redis://localhost:6379
`,
		RunE: runHARelayer,
	}

	cmd.Flags().String(flagRelayerConfig, "", "Path to HA relayer config file (required)")
	cmd.Flags().String(flagRedisURL, "redis://localhost:6379", "Redis connection URL")
	cmd.Flags().Bool(flagStrictConfig, false, "Refuse to start when the config carries keys this binary does not understand (default: warn and start)")

	_ = cmd.MarkFlagRequired(flagRelayerConfig)

	cmd.AddCommand(relayerValidateCmd())

	return cmd
}

const (
	flagCheckStake = "check-stake"
	flagNode       = "node"
)

// relayerValidateCmd runs the exact config checks the relayer runs at startup —
// parse, Validate(), and BuildPools() — without starting anything, and exits
// non-zero on the first error. Run it before deploying a config change: a
// config the relayer rejects will not boot, so catching it here beats finding
// out when the pod fails to come up.
//
// With --check-stake it additionally queries the chain for each configured
// supplier's on-chain service configs and cross-checks the staked
// (service, transport) pairs against the local backends: a pair you are staked
// for but have no backend to serve is an ERROR (you earn nothing on it,
// silently); a backend you configured that no supplier stakes is surplus,
// reported as info.
func relayerValidateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a relayer config without starting the relayer",
		Long: `Validate a relayer config against the same checks the relayer runs at startup.

Exits 0 if the config would boot, non-zero with the first error otherwise.
Run this before rolling out a config change — a config the relayer rejects
(e.g. an unknown backend key like "ws" instead of "websocket", or a websocket
backend with an http:// url) will not start the process.

With --check-stake it also queries the chain (using pocket_node.query_node_grpc_url
from the config, or --node to override) for each configured supplier's staked
(service, transport) pairs and reports any you cannot serve — the misconfiguration
that earns nothing, silently.

Examples:
  pocket-relay-miner relayer validate --config /path/to/relayer.yaml
  pocket-relay-miner relayer validate --config /path/to/relayer.yaml --check-stake`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			initSDKConfig()
			configPath, _ := cmd.Flags().GetString(flagRelayerConfig)
			config, err := relayer.LoadConfig(configPath)
			if err != nil {
				return fmt.Errorf("config is INVALID: %w", err)
			}
			// Validating IS this command's job, so a key the relayer does not
			// understand is a failure here, with no flag involved. The serving
			// binary makes the friendlier choice (warn and start, unless
			// --strict-config); this is the door an operator walks through
			// deliberately, before the rollout, to be told everything at once.
			//
			// Returned rather than printed: cobra renders it and sets a non-zero
			// exit, which is what a pipeline reads.
			if unknown := config.Warnings(); len(unknown) > 0 {
				return fmt.Errorf(
					"config is INVALID: %d key(s) this relayer does not understand:\n  %s",
					len(unknown), strings.Join(unknown, "\n  "))
			}

			fmt.Printf("config OK: %s would start\n", configPath)

			// A disabled simulation block is skipped by Validate, by design.
			// Report what enabling it would do anyway: otherwise the operator
			// finds out on the deploy that flips the switch, when the relayer
			// refuses to boot and stops serving real traffic. Advisory only —
			// exit status stays 0.
			//
			// Only when identities are actually pinned: the shipped default is
			// disabled with none, and warning about that would fire on every
			// correct config.
			if !config.Simulation.Enabled && len(config.Simulation.Identities) > 0 {
				if simErr := config.Simulation.ValidateAsIfEnabled(); simErr != nil {
					fmt.Printf("warning: simulation is disabled, so its config was not checked; "+
						"it would be REJECTED if you set simulation.enabled: true: %v\n", simErr)
				}
			}

			// An expired identity is valid config that serves nothing. Report
			// it here rather than let a health-check pipeline go quiet with no
			// explanation. Advisory by design — see ExpiredIdentities. Not
			// gated on simulation.enabled: this command is a look-ahead, and
			// an identity that is already dead is worth knowing about before
			// the deploy that switches the feature on, not after.
			if expired := config.Simulation.ExpiredIdentities(time.Now()); len(expired) > 0 {
				fmt.Printf("warning: simulation identities are past their not_after and will reject every relay: %s\n",
					strings.Join(expired, ", "))
			}

			checkStake, _ := cmd.Flags().GetBool(flagCheckStake)
			if !checkStake {
				return nil
			}
			nodeOverride, _ := cmd.Flags().GetString(flagNode)
			return runCheckStake(cmd.Context(), config, nodeOverride)
		},
	}
	cmd.Flags().String(flagRelayerConfig, "", "Path to HA relayer config file (required)")
	cmd.Flags().Bool(flagCheckStake, false, "Cross-check on-chain stake against configured backends (queries the chain)")
	cmd.Flags().String(flagNode, "", "Override the gRPC query node URL (default: pocket_node.query_node_grpc_url from config)")
	_ = cmd.MarkFlagRequired(flagRelayerConfig)
	return cmd
}

// runCheckStake queries the chain for every configured supplier's staked
// (service, transport) pairs and cross-checks them against the local relayer
// backends. It returns a non-nil error (non-zero exit) when any staked pair has
// no backend to serve it.
func runCheckStake(ctx context.Context, config *relayer.Config, nodeOverride string) error {
	logger := logging.NewLoggerFromConfig(config.Logging)

	addresses, err := loadConfiguredSupplierAddresses(ctx, logger, config.Keys)
	if err != nil {
		return fmt.Errorf("--check-stake: failed to load supplier keys: %w", err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("--check-stake: no supplier keys configured (keys_file/keyring); nothing to check")
	}

	grpcURL := config.PocketNode.QueryNodeGRPCUrl
	if nodeOverride != "" {
		grpcURL = nodeOverride
	}
	if grpcURL == "" {
		return fmt.Errorf("--check-stake: no gRPC query node (set pocket_node.query_node_grpc_url or pass --node)")
	}
	grpcEndpoint := stripGRPCScheme(grpcURL)

	queryClients, err := query.NewQueryClients(logger, query.ClientConfig{
		GRPCEndpoint: grpcEndpoint,
		QueryTimeout: 30 * time.Second,
		UseTLS:       !config.PocketNode.GRPCInsecure,
	})
	if err != nil {
		return fmt.Errorf("--check-stake: failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()

	fmt.Printf("\nchecking on-chain stake for %d supplier(s) via %s (tls=%t)\n",
		len(addresses), grpcEndpoint, !config.PocketNode.GRPCInsecure)

	var staked []relayer.StakedPair
	var notStaked []string
	for _, addr := range addresses {
		supplier, qErr := queryClients.Supplier().GetSupplier(ctx, addr)
		if qErr != nil {
			// A key with no on-chain supplier record is simply not staked yet —
			// info, not a fault. Only real transport/query errors abort, so a
			// single missing registration cannot hide the gaps of the suppliers
			// that ARE staked.
			if isSupplierNotFoundError(qErr) {
				notStaked = append(notStaked, addr)
				continue
			}
			return fmt.Errorf("--check-stake: failed to query supplier %s: %w", addr, qErr)
		}
		staked = append(staked, relayer.ExtractStakedPairs(addr, supplier.Services)...)
	}

	report := relayer.CrossCheckStake(config, staked)
	printStakeReport(report, notStaked)

	if report.HasErrors() {
		return fmt.Errorf("--check-stake: %d staked (service, transport) pair(s) have no backend — you earn nothing on them", len(report.Missing))
	}
	return nil
}

// isSupplierNotFoundError reports whether a supplier query error means the
// operator address is not staked on-chain (no supplier record), as opposed to a
// transport/query failure that could not answer either way.
//
// Follows query.IsEntityNotFound's policy (explicit gRPC NotFound only) so this
// report never downgrades an unreachable node into "not staked, nothing to serve"
// — which would hide a real unserved stake behind an info line.
func isSupplierNotFoundError(err error) bool {
	return query.IsEntityNotFound(err)
}

// printStakeReport writes the cross-check outcome to stdout: staked pairs with
// no backend as errors, surplus backends and unstaked suppliers as info, and
// unmappable rpc_types as a debug note.
func printStakeReport(report relayer.StakeCheckReport, notStaked []string) {
	for _, addr := range notStaked {
		fmt.Printf("  info   supplier not staked on-chain (nothing to serve): supplier=%s\n", addr)
	}
	for _, m := range report.Missing {
		fmt.Printf("  ERROR  staked but no backend: supplier=%s service=%s transport=%s\n",
			m.Supplier, m.ServiceID, m.BackendType)
	}
	for _, e := range report.Extra {
		fmt.Printf("  info   backend configured, not staked (surplus): service=%s transport=%s\n",
			e.ServiceID, e.BackendType)
	}
	for _, u := range report.Unknown {
		fmt.Printf("  debug  on-chain transport this build cannot map (skipped): supplier=%s service=%s rpc_type=%s\n",
			u.Supplier, u.ServiceID, u.RPCType.String())
	}
	if report.HasErrors() {
		fmt.Printf("stake check FAILED: %d unserved staked pair(s)\n", len(report.Missing))
	} else {
		fmt.Printf("stake check OK: every staked (service, transport) pair has a backend\n")
	}
}

// loadConfiguredSupplierAddresses builds the configured key providers, loads
// every key, and returns the sorted unique operator addresses. Providers are
// closed before returning.
func loadConfiguredSupplierAddresses(ctx context.Context, logger logging.Logger, kc sharedconfig.KeysConfig) ([]string, error) {
	providers, err := buildKeyProviders(logger, kc)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, p := range providers {
			_ = p.Close()
		}
	}()

	addrSet := make(map[string]struct{})
	for _, provider := range providers {
		loaded, loadErr := provider.LoadKeys(ctx)
		if loadErr != nil {
			return nil, fmt.Errorf("provider %s: %w", provider.Name(), loadErr)
		}
		for addr := range loaded {
			addrSet[addr] = struct{}{}
		}
	}

	addresses := make([]string, 0, len(addrSet))
	for addr := range addrSet {
		addresses = append(addresses, addr)
	}
	sort.Strings(addresses)
	return addresses, nil
}

// buildKeyProviders constructs the configured key providers (keys_file,
// keyring) in the same order and with the same logging as the relayer
// boot path. Callers own closing the returned providers.
func buildKeyProviders(logger logging.Logger, kc sharedconfig.KeysConfig) ([]keys.KeyProvider, error) {
	return keys.BuildProviders(logger, kc.KeysFile, keyringSettings(kc.Keyring))
}

// keyringSettings maps a configured keyring onto what keys.BuildProviders needs,
// or nil when no keyring is configured.
func keyringSettings(kr *sharedconfig.KeyringConfig) *keys.KeyringSettings {
	if kr == nil || kr.Backend == "" {
		return nil
	}
	return &keys.KeyringSettings{
		Backend:        kr.Backend,
		Dir:            kr.Dir,
		AppName:        kr.AppName,
		KeyNames:       kr.KeyNames,
		PassphraseFile: kr.PassphraseFile,
		PassphraseEnv:  kr.PassphraseEnv,
	}
}

// supplierSigningKeys snapshots the key manager's current key set.
//
// KeyManager exposes ListSuppliers plus GetSigner rather than the map itself, so
// the map is rebuilt here. A key removed between the two calls is skipped rather
// than treated as an error: the reload that removed it fires its own change
// callback, and that callback takes a fresh snapshot.
func supplierSigningKeys(km keys.KeyManager) map[string]cryptotypes.PrivKey {
	addresses := km.ListSuppliers()
	signingKeys := make(map[string]cryptotypes.PrivKey, len(addresses))
	for _, addr := range addresses {
		key, err := km.GetSigner(addr)
		if err != nil {
			continue
		}
		signingKeys[addr] = key
	}
	return signingKeys
}

// stripGRPCScheme removes a URL scheme prefix from a gRPC endpoint, matching the
// relayer boot path — grpc.NewClient wants host:port, not a scheme.
func stripGRPCScheme(grpcURL string) string {
	for _, scheme := range []string{"grpcs://", "grpc://", "https://", "http://"} {
		grpcURL = strings.TrimPrefix(grpcURL, scheme)
	}
	return grpcURL
}

// serviceDifficultyQueryAdapter adapts the query.ServiceDifficultyClient to the
// relayer.ServiceDifficultyQueryClient interface by extracting TargetHash.
// It uses the height-aware difficulty query to get difficulty at the session start height,
// ensuring consistency with on-chain proof validation.
type serviceDifficultyQueryAdapter struct {
	queryClient query.ServiceDifficultyClient
}

func (a *serviceDifficultyQueryAdapter) GetServiceRelayDifficulty(ctx context.Context, serviceID string, sessionStartHeight int64) ([]byte, error) {
	difficulty, err := a.queryClient.GetServiceRelayDifficultyAtHeight(ctx, serviceID, sessionStartHeight)
	if err != nil {
		return nil, err
	}
	return difficulty.TargetHash, nil
}

func runHARelayer(cmd *cobra.Command, _ []string) error {
	// Initialize Cosmos SDK config with "pokt" Bech32 prefix
	// This must be done before any address validation occurs
	initSDKConfig()

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// Load config first (needed for logger configuration)
	configPath, _ := cmd.Flags().GetString(flagRelayerConfig)
	config, err := relayer.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Set up logger from config
	logger := logging.NewLoggerFromConfig(config.Logging)
	memlimit.Apply(logger)

	// Keys the file carries that this binary does not understand.
	//
	// Warn and start, by default and on purpose: the ConfigMap and the binary
	// roll out separately, so a new binary landing beside an older config is the
	// NORMAL case of a rolling deploy, not an anomaly. Refusing to boot there
	// converts a stale key into an outage. Loading a config change is a state
	// change, not a per-request event, so Warn is the right level.
	//
	// --strict-config is for the operator who wants the guarantee instead: same
	// finding, fatal. `relayer validate` is always strict, with no flag, because
	// validating is what that command is for.
	unknown := config.Warnings()
	for _, w := range unknown {
		logger.Warn().Msg(w)
	}
	if len(unknown) > 0 {
		if strict, _ := cmd.Flags().GetBool(flagStrictConfig); strict {
			return fmt.Errorf(
				"--strict-config: refusing to start, %d key(s) this relayer does not understand (listed above)",
				len(unknown))
		}
	}

	// Start observability server (metrics and pprof)
	if config.Metrics.Enabled || config.Pprof.Enabled {
		// Default pprof addr to localhost:6060 for security if not specified
		pprofAddr := config.Pprof.Addr
		if pprofAddr == "" {
			pprofAddr = "localhost:6060"
		}

		// Combine RelayerRegistry and SharedRegistry so cache metrics are exposed
		combinedRegistry := prometheus.Gatherers{
			observability.RelayerRegistry,
			observability.SharedRegistry,
		}

		obsServer := observability.NewServer(logger, observability.ServerConfig{
			MetricsEnabled: config.Metrics.Enabled,
			MetricsAddr:    config.Metrics.Addr,
			PprofEnabled:   config.Pprof.Enabled,
			PprofAddr:      pprofAddr,
			Registry:       combinedRegistry,
		})
		if err := obsServer.Start(ctx); err != nil {
			return fmt.Errorf("failed to start observability server: %w", err)
		}
		defer func() { _ = obsServer.Stop() }() //nolint:errcheck // Stop logs every shutdown failure at Error before returning the last one; this deferred caller has nobody to hand it to
		logger.Info().Str("addr", config.Metrics.Addr).Msg("observability server started")

		// Start runtime metrics collector
		runtimeMetrics := observability.NewRuntimeMetricsCollector(
			logger,
			observability.DefaultRuntimeMetricsCollectorConfig(),
			observability.RelayerFactory,
		)
		if err := runtimeMetrics.Start(ctx); err != nil {
			return fmt.Errorf("failed to start runtime metrics collector: %w", err)
		}
		defer runtimeMetrics.Stop()
		logger.Info().Msg("runtime metrics collector started")
	}

	// The relayer's concurrency budget, computed ONCE here and read by both the
	// Redis pool below and the master worker pool further down. They describe
	// the same thing -- how much of this process can be inside a Redis call at
	// once -- and computing them separately is how they drifted apart.
	//
	// GOMAXPROCS(0), not NumCPU(): NumCPU reports the machine's cores and
	// ignores the container's CPU limit. automaxprocs sets GOMAXPROCS from the
	// cgroup quota at startup, so this is what the runtime will schedule on.
	sizing := relayer.ComputeWorkerSizingForProcess()

	// An operator value wins; otherwise the pool follows the workers.
	redisPoolSize := config.Redis.PoolSize
	if redisPoolSize <= 0 {
		redisPoolSize = sizing.RedisPoolSize()
	}

	// Use Redis URL from config, allow flag override
	redisURL := config.Redis.URL
	if cmd.Flags().Changed(flagRedisURL) {
		redisURL, _ = cmd.Flags().GetString(flagRedisURL)
	}

	// Create wrapped Redis client with KeyBuilder for namespace-aware key construction
	redisClient, err := redistransport.NewClient(ctx, redistransport.ClientConfig{
		URL:                    redisURL,
		PoolSize:               redisPoolSize,
		MinIdleConns:           config.Redis.MinIdleConns,
		PoolTimeoutSeconds:     config.Redis.PoolTimeoutSeconds,
		ConnMaxIdleTimeSeconds: config.Redis.ConnMaxIdleTimeSeconds,
		Namespace:              config.Redis.Namespace,
	})
	if err != nil {
		return fmt.Errorf("failed to create Redis client: %w", err)
	}
	defer func() { _ = redisClient.Close() }()
	logger.Info().Str("redis_url", redisURL).Msg("connected to Redis")

	// Whether Redis can take writes, answered once for the whole relayer: every
	// client this process writes through reports refused writes to it, and every
	// admission path reads it.
	storeHealth := redistransport.NewStoreHealth(logger, redisClient.UniversalClient, "relayer", redistransport.StoreGateAdmission)
	redisClient.AddHook(storeHealth.Hook())
	storeHealth.Start(ctx)

	// Redis pool statistics. Registered HERE and not in NewClient: fifteen test
	// files and the redis CLI build clients, and a repeated MustRegister panics.
	// The collector is also the registry of pools, so a client per supplier can
	// be added and removed as suppliers are adopted and released.
	// What the pool ACTUALLY holds, published from the client. See
	// RegisterEffectivePoolGauges: reading the config here would certify the
	// request rather than what runs, and the pool timeout is precisely the
	// value nobody sets and go-redis defaults behind our backs.
	if gaugeErr := redistransport.RegisterEffectivePoolGauges(
		observability.SharedRegistry, "relayer", redisClient,
	); gaugeErr != nil {
		return fmt.Errorf("failed to register effective Redis pool gauges: %w", gaugeErr)
	}

	// The pool has to cover the workers that will use it. This compares the
	// EFFECTIVE size -- what the client holds, not what we asked for -- against
	// the bounded users, and refuses to start rather than discovering it under
	// load as a queue nobody can explain.
	//
	// WHAT THIS GUARD DOES NOT COVER: in eager validation mode the relay meter
	// runs inline in the HTTP handler, not inside a subpool, so its concurrency
	// is whatever the HTTP server admits and no startup number can bound it.
	// This guard covers the BOUNDED users; the pool metrics show the rest. Read
	// it as "the floor is right", never as "the pool is sufficient".
	if eff, ok := redisClient.EffectivePoolOptions(); ok {
		needed := sizing.Validation + sizing.Publish
		if eff.PoolSize < needed {
			return fmt.Errorf(
				"redis pool too small: the client holds %d connections but the bounded workers that use it "+
					"need %d (validation %d + publish %d); raise redis.pool_size or lower the worker count",
				eff.PoolSize, needed, sizing.Validation, sizing.Publish)
		}
		logger.Info().
			Int("pool_size_effective", eff.PoolSize).
			Int("min_idle_conns_effective", eff.MinIdleConns).
			Dur("pool_timeout_effective", eff.PoolTimeout).
			Int("bounded_workers", needed).
			Msg("Redis pool covers the bounded workers (the eager meter path is NOT bounded by this)")
	} else {
		logger.Warn().
			Msg("could not read the effective Redis pool settings from this client type: " +
				"the startup pool guard did NOT run")
	}

	// How long each Redis command really takes from here, pool wait and
	// go-redis retries included. The pool's own wait series cannot answer that:
	// they count only waits that ended in a connection, so their mean improves
	// as the pool starts failing.
	redisClient.AddHook(redistransport.NewCommandLatencyHook("relayer"))

	redisPools := redistransport.NewPoolCollector("relayer")
	redisPools.Add("shared", redisClient)
	observability.SharedRegistry.MustRegister(redisPools)

	// Create supplier cache for checking supplier staking state
	supplierCache := cache.NewSupplierCache(
		logger,
		redisClient,
		cache.SupplierCacheConfig{},
	)
	// Start supplier cache for pub/sub subscription
	if err := supplierCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start supplier cache: %w", err)
	}
	defer func() { _ = supplierCache.Close() }()
	logger.Info().Msg("supplier cache initialized and started")

	// Warmup supplier cache from Redis (load existing suppliers into L1 cache)
	// This prevents relying solely on pub/sub, which can miss messages if relayer starts after miner
	if err := supplierCache.WarmupFromRedis(ctx, nil); err != nil {
		return fmt.Errorf("failed to warmup supplier cache: %w", err)
	}

	// Create query clients for fetching on-chain data (service compute units, etc.)
	// Determine TLS setting - use config.PocketNode.GRPCInsecure to match miner behavior
	grpcURL := config.PocketNode.QueryNodeGRPCUrl
	// Strip scheme for gRPC endpoint
	grpcEndpoint := grpcURL
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpcs://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "grpc://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "https://")
	grpcEndpoint = strings.TrimPrefix(grpcEndpoint, "http://")

	queryClients, err := query.NewQueryClients(
		logger,
		query.ClientConfig{
			GRPCEndpoint: grpcEndpoint,
			QueryTimeout: 30 * time.Second,
			UseTLS:       !config.PocketNode.GRPCInsecure,
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create query clients: %w", err)
	}
	defer func() { _ = queryClients.Close() }()
	logger.Info().Str("grpc_endpoint", grpcEndpoint).Bool("tls", !config.PocketNode.GRPCInsecure).Msg("query clients initialized")

	// Create new entity caches (relayer only subscribes to pub/sub, doesn't refresh)
	// These caches provide L1/L2/L3 pattern with pub/sub invalidation

	// Relayer uses default 30s block time (production default for mainnet/testnet)
	blockTimeSeconds := int64(30)

	// NOTE: session params are read live via the session query client (90s TTL) in
	// RelayMeter.getSessionParams — the relayer does NOT use a session-params
	// singleton (it ran no orchestrator to refresh it and never read it).

	// Create shared params cache with dynamic 2-session TTL
	sharedParamsCache := cache.NewSharedParamsCache(
		logger,
		redisClient,
		queryClients.Shared(),
		blockTimeSeconds,
	)
	if err := sharedParamsCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start shared params cache: %w", err)
	}
	defer func() { _ = sharedParamsCache.Close() }()

	// Create application cache
	applicationCache := cache.NewApplicationCache(
		logger,
		redisClient,
		cache.NewApplicationQueryClientAdapter(queryClients.Application()),
	)
	if err := applicationCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start application cache: %w", err)
	}
	defer func() { _ = applicationCache.Close() }()

	// Create service cache
	serviceCache := cache.NewServiceCache(
		logger,
		redisClient,
		cache.NewServiceQueryClientAdapter(queryClients.Service()),
	)
	if err := serviceCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start service cache: %w", err)
	}
	defer func() { _ = serviceCache.Close() }()

	// Create account cache for public key lookups (used by ring client)
	// IMPORTANT: Public keys are immutable, so this cache has NO EXPIRY
	accountCache := cache.NewAccountCache(
		logger,
		redisClient,
		queryClients.Account(),
	)
	if err := accountCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start account cache: %w", err)
	}
	defer func() { _ = accountCache.Close() }()

	logger.Info().Msg("entity caches started (pub/sub subscribers)")

	// Cache warmup: Pre-warm L1 caches from L2 (Redis) for known applications
	// This significantly speeds up cold starts by avoiding L3 (chain) queries during first relays
	if config.CacheWarmup.Enabled {
		logger.Info().
			Int("known_apps", len(config.CacheWarmup.KnownApplications)).
			Int("concurrency", config.CacheWarmup.WarmupConcurrency).
			Msg("starting cache warmup for faster cold starts")

		// Create cache warmer
		cacheWarmer := cache.NewCacheWarmer(
			logger,
			cache.CacheWarmerConfig{
				KnownApplications: config.CacheWarmup.KnownApplications,
				WarmupConcurrency: config.CacheWarmup.WarmupConcurrency,
				WarmupTimeout:     time.Duration(config.CacheWarmup.WarmupTimeoutSeconds) * time.Second,
			},
			queryClients.Application(),
			queryClients.Account(),
			queryClients.Shared(),
		)
		// Ensure worker pool cleanup
		defer cacheWarmer.Stop()

		// Execute warmup (this populates L1 cache from L2/L3)
		warmupCtx, warmupCancel := context.WithTimeout(ctx, 30*time.Second)
		warmupResult, err := cacheWarmer.Warmup(warmupCtx)
		warmupCancel()

		if err != nil {
			logger.Warn().Err(err).Msg("cache warmup failed (continuing anyway)")
		} else {
			logger.Info().
				Int("total_apps", warmupResult.TotalApps).
				Int("warmed", warmupResult.WarmedApps).
				Int("failed", warmupResult.FailedApps).
				Int64("duration_ms", warmupResult.DurationMs).
				Msg("cache warmup completed - relayer ready with pre-warmed caches")
		}
	} else {
		logger.Info().Msg("cache warmup disabled - caches will populate on-demand during relays")
	}

	// Verify RPC endpoint connectivity via HTTP /status (health check)
	rpcURL := config.PocketNode.QueryNodeRPCUrl
	if err := verifyRPCConnectivity(ctx, logger, rpcURL); err != nil {
		logger.Warn().Err(err).Str("rpc_url", rpcURL).Msg("RPC health check failed (continuing anyway)")
	} else {
		logger.Info().Str("rpc_url", rpcURL).Msg("RPC endpoint connectivity verified")
	}

	// Verify gRPC endpoint connectivity (health check)
	if err = verifyGRPCConnectivity(ctx, logger, grpcURL, queryClients); err != nil {
		logger.Warn().Err(err).Str("grpc_url", grpcURL).Msg("gRPC health check failed (continuing anyway)")
	} else {
		logger.Info().Str("grpc_url", grpcURL).Msg("gRPC endpoint connectivity verified")
	}

	// Create Redis block subscriber to receive block events from miner
	// Relayers use Redis pub/sub for block synchronization (no WebSocket connections).
	redisBlockSubscriber := cache.NewRedisBlockSubscriber(
		logger,
		redisClient,
		nil, // No direct blockchain client - events come from miner via Redis
	)
	if err := redisBlockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start redis block subscriber: %w", err)
	}
	defer func() { _ = redisBlockSubscriber.Close() }()
	logger.Info().Msg("redis block subscriber started (receiving events from miner)")

	// Create RedisBlockClientAdapter to implement client.BlockClient interface
	// This adapter receives events from Redis pub/sub and provides the BlockClient
	// interface required by relayer components (proxy, relay meter, etc.)
	// Relayers don't need to query specific blocks, so pass nil for RPC client
	blockSubscriber := cache.NewRedisBlockClientAdapter(
		logger,
		redisBlockSubscriber,
		nil, // Relayers only need block events, not specific block queries
	)
	// Register this binary as a BlockEvents() consumer BEFORE Start() begins
	// forwarding. The adapter discards block events on arrival until a consumer
	// has wired up BlockEvents() (so the miner, which never consumes it, doesn't
	// buffer/warn); calling it here — rather than lazily inside the consumer
	// goroutine below — closes the startup window where an early block could be
	// dropped before the flag flips.
	blockEventsCh := blockSubscriber.BlockEvents()
	if err := blockSubscriber.Start(ctx); err != nil {
		return fmt.Errorf("failed to start block client adapter: %w", err)
	}
	defer func() { blockSubscriber.Close() }()
	logger.Info().Msg("block client adapter started (Redis-backed)")

	// NOTE: Cache warmup is now handled by the miner's CacheOrchestrator.
	// The relayer's ApplicationCache.WarmupFromRedis() will populate L1 from L2 on startup.
	// Discovery of apps/services happens on the miner side when processing relays from Redis streams.

	// Create publisher for mined relays.
	//
	// No TTL is passed: relay streams do not expire. relay_meter.cache_ttl still
	// governs the meter's own per-session keys further down; it used to double as
	// the stream's lifetime, which deleted un-consumed relays mid-session.
	//
	// Always batched: one MULTI/EXEC per interval instead of one round trip per
	// relay, which wakes the miner's blocked reader once per batch. Only the
	// interval is configurable.
	//
	// The batches write through a Redis client of their own, so a busy cache or
	// meter cannot hold the dispatch back on a shared pool: one connection per
	// dispatch worker, plus one for the heartbeat PING sent while the queue is
	// empty.
	batchWorkers := relayer.BatchDispatchWorkersForProcess()
	batchRedisClient, err := redistransport.NewClient(ctx, redistransport.ClientConfig{
		URL:                    redisURL,
		PoolSize:               batchWorkers + 1,
		MinIdleConns:           batchWorkers + 1,
		PoolTimeoutSeconds:     config.Redis.PoolTimeoutSeconds,
		ConnMaxIdleTimeSeconds: config.Redis.ConnMaxIdleTimeSeconds,
		Namespace:              config.Redis.Namespace,
	})
	if err != nil {
		return fmt.Errorf("failed to create the batch dispatch Redis client: %w", err)
	}
	// Declared before the publisher's deferred Close, so it runs after it: the
	// final flush writes through this client.
	defer func() { _ = batchRedisClient.Close() }()
	if gaugeErr := redistransport.RegisterEffectivePoolGauges(
		observability.SharedRegistry, "relayer_batch", batchRedisClient,
	); gaugeErr != nil {
		return fmt.Errorf("failed to register effective Redis pool gauges for the batch client: %w", gaugeErr)
	}
	if eff, ok := batchRedisClient.EffectivePoolOptions(); ok && eff.PoolSize < batchWorkers+1 {
		return fmt.Errorf(
			"batch dispatch redis pool too small: the client holds %d connections but %d dispatch workers need %d",
			eff.PoolSize, batchWorkers, batchWorkers+1)
	}
	batchRedisClient.AddHook(redistransport.NewCommandLatencyHook("relayer_batch"))
	batchRedisClient.AddHook(storeHealth.Hook())
	redisPools.Add("batch", batchRedisClient)

	batcher := redistransport.NewBatchingPublisher(
		logger,
		batchRedisClient.UniversalClient, // the dispatch's own client, not the shared pool
		redisClient.KB().StreamPrefix(),  // Namespace-aware stream prefix (e.g., "ha:relays")
		config.Redis.BatchPublishInterval(),
		redistransport.WithDispatchWorkers(batchWorkers),
		redistransport.WithStoreHealth(storeHealth),
	)
	var publisher transport.MinedRelayPublisher = batcher
	logger.Info().Dur("interval", config.Redis.BatchPublishInterval()).Msg("batched relay publishing")
	// Before both Redis clients' deferred Close (declared earlier, so they run
	// after this one): the final flush writes through the batch client, and
	// closing it first would lose whatever the batch still held.
	defer func() { _ = publisher.Close() }()

	// Create health checker
	healthChecker := relayer.NewHealthChecker(logger)

	// Create master worker pool for controlled concurrency
	// Uses unbounded queue with non-blocking submission to prevent goroutine explosion
	masterPoolSize := sizing.Master
	masterPool := pond.NewPool(
		masterPoolSize,
		pond.WithQueueSize(pond.Unbounded),
		pond.WithNonBlocking(true),
	)
	defer masterPool.StopAndWait()
	// Both numbers on purpose: when they differ, the pod has a CPU limit below
	// the node's cores and gomaxprocs is the one that decided the workers. An
	// operator reading only num_cpu would compute a pool size this process is
	// not using.
	logger.Info().
		Int("max_workers", masterPoolSize).
		Int("validation_workers", sizing.Validation).
		Int("publish_workers", sizing.Publish).
		Int("metrics_workers", sizing.Metrics).
		Int("redis_pool_size", redisPoolSize).
		Int("gomaxprocs", runtime.GOMAXPROCS(0)).
		Int("num_cpu", runtime.NumCPU()).
		Msg("created master worker pool (unbounded, non-blocking, 8x GOMAXPROCS)")

	// Create proxy server
	proxy, err := relayer.NewProxyServer(
		logger,
		config,
		publisher,
		masterPool, // Pass master worker pool
	)
	if err != nil {
		return fmt.Errorf("failed to create proxy server: %w", err)
	}

	// Admission stops while the batch holds more than redis.batch_max_queued_mib.
	// The gate reads the CONCRETE batcher, never the publisher the proxy wraps in
	// countPublished: one path, independent of decorator order.
	maxQueuedBytes := config.Redis.BatchMaxQueuedBytes()
	proxy.SetPublishQueueFull(func() bool { return batcher.QueuedBytes() >= maxQueuedBytes })
	// Redis being able to take writes is the first gate of every transport.
	proxy.SetStoreHealth(storeHealth)

	// Event-driven block height updates (replaces 1s polling)
	// Receives block events from Redis pub/sub for ~1-2ms latency (vs 1s polling)
	go func() {
		logger.Info().Msg("using event-driven block updates from Redis")
		for {
			select {
			case <-ctx.Done():
				return
			case block, ok := <-blockEventsCh:
				if !ok {
					// Channel closed
					logger.Warn().Msg("block events channel closed")
					return
				}
				proxy.SetBlockHeight(block.Height())
			}
		}
	}()
	logger.Info().Msg("proxy subscribed to block height updates from Redis")

	// Load keys and create response signer
	// Support multiple key sources: keys_file, keyring
	// One shared sequence for both binaries: build the providers the config
	// names, put a key manager over them, load once, arm the watch and the
	// reload timer, and refuse to continue with no keys. See keys.OpenManager
	// for why that lives there and not here.
	keyManager, err := keys.OpenManager(
		ctx, logger,
		config.Keys.KeysFile,
		keyringSettings(config.Keys.Keyring),
		config.Keys.HotReloadEnabled,
	)
	if err != nil {
		return err
	}
	defer func() { _ = keyManager.Close() }()

	loadedKeys := supplierSigningKeys(keyManager)

	responseSigner, signerErr := relayer.NewResponseSigner(logger, loadedKeys)
	if signerErr != nil {
		return fmt.Errorf("failed to create response signer: %w", signerErr)
	}
	proxy.SetResponseSigner(responseSigner)

	// One reload can add keys and remove keys at once, and the manager
	// reports each changed address separately. The whole set is rebuilt
	// on every callback rather than patched: replacing it is atomic, so
	// a supplier the reload did not touch is never briefly without a
	// signer, and applying N changes N times is idempotent.
	//
	// Added and removed is the whole space of changes here, because
	// every provider derives the operator address FROM the key material
	// -- see the diff in MultiProviderKeyManager.Reload for why a key
	// cannot change behind an address that stays.
	// Rebuilding is read-then-store, so it has to be serialised with itself: two
	// rebuilds racing can store in the opposite order to the one they read in,
	// and the older set wins. That is not hypothetical here -- the startup
	// re-read below runs while the reload timer is already ticking.
	var signerSync sync.Mutex
	resyncSigner := func() int {
		signerSync.Lock()
		defer signerSync.Unlock()
		responseSigner.ReplaceKeys(supplierSigningKeys(keyManager))
		n := len(responseSigner.GetOperatorAddresses())
		// Set here rather than in the OnKeyChange callback so the STARTUP
		// resync publishes it too: a gauge that only appears after the first
		// reload reads as zero keys on a fleet that never changes.
		relayer.SetSigningKeysLoaded(n)
		return n
	}

	keyManager.OnKeyChange(func(operatorAddr string, added bool) {
		keptKeys := resyncSigner()
		logger.Info().
			Str("operator_address", operatorAddr).
			Bool("added", added).
			Int("keys", keptKeys).
			Msg("signing key change applied to the running relayer")
	})

	// Re-read AFTER registering, because the manager returned from OpenManager
	// is already running: a reload landing between the snapshot above and this
	// registration would update the manager's keys and find no callback to
	// notify, leaving the signer on the pre-reload set until the NEXT change --
	// which on a steady fleet may never come. Rebuilding the whole set is
	// idempotent, so paying for one extra read at startup closes the window.
	resyncSigner()

	// Wire the simulated-relay verifier (Admission zone). Optional and
	// off by default; when disabled the header is ignored. Needs the
	// response signer (to sign simulated responses) and the set of
	// configured service IDs (to bind a simulated relay to a real,
	// routable service).
	simServiceIDs := make(map[string]struct{}, len(config.Services))
	for svcID := range config.Services {
		simServiceIDs[svcID] = struct{}{}
	}
	simVerifier, simErr := relayer.NewSimulationVerifier(
		logger, &config.Simulation, redisClient, responseSigner, simServiceIDs, nil,
	)
	if simErr != nil {
		return fmt.Errorf("failed to create simulation verifier: %w", simErr)
	}
	proxy.SetSimulationVerifier(simVerifier)
	if config.Simulation.Enabled {
		logger.Info().
			Int("identities", len(config.Simulation.Identities)).
			Msg("simulated relays ENABLED")
		// Valid config that serves nothing: the identity loads and
		// then rejects every relay with ErrSimExpired. Never fatal —
		// see SimulationConfig.ExpiredIdentities.
		if expired := config.Simulation.ExpiredIdentities(time.Now()); len(expired) > 0 {
			logger.Warn().
				Strs("key_ids", expired).
				Msg("simulated relay identities are past their not_after: they will reject every relay")
		}
	} else if len(config.Simulation.Identities) > 0 {
		// The block is off, so it cannot affect this process — but it
		// may still be unbootable, and the next deploy that enables it
		// would crashloop this replica. Say so now, while it is cheap.
		// Identities must be present: the shipped default is disabled
		// with none, and warning about that would fire on every
		// correct config.
		if simCfgErr := config.Simulation.ValidateAsIfEnabled(); simCfgErr != nil {
			logger.Warn().
				Err(simCfgErr).
				Msg("simulation is disabled and its config is invalid: enabling it would prevent startup")
		}
	}

	logger.Info().
		Int("num_keys", len(responseSigner.GetOperatorAddresses())).
		Msg("response signer initialized")

	// Create RingClient for relay request signature verification
	// This is critical for security - it validates that relay requests
	// are properly signed by the application or a delegated gateway
	//
	// IMPORTANT: Use Redis-backed caches for all queries to minimize latency:
	// - Application cache: L1→L2→L3 for app delegation info
	// - Account cache: L1→L2→L3 for public key lookups (NO EXPIRY - keys are immutable)
	// - Shared params cache: L1→L2→L3 for session parameters
	//
	// At 1000 RPS, this prevents:
	// - 1000+ application queries/sec to blockchain
	// - 2000-4000 public key queries/sec to blockchain (N keys per ring)
	// - 1000+ shared params queries/sec to blockchain
	ringClient := rings.NewRingClient(
		logger,
		cache.NewCachedApplicationQueryClient(applicationCache),
		cache.NewCachedAccountQueryClient(accountCache),
		cache.NewCachedSharedQueryClient(sharedParamsCache, queryClients.Shared()),
	)

	// Create caches for full session validation
	//
	// BlockTimeSeconds has no operator-configurable field on this path today
	// (unlike the miner's block_time_seconds) -- cache.DefaultBlockTimeSeconds
	// is the single source for that fallback value across the whole process;
	// see its doc comment for why the value itself is not "corrected" here.
	cacheConfig := cache.CacheConfig{
		TTLBlocks:        1,
		BlockTimeSeconds: cache.DefaultBlockTimeSeconds,
	}

	// Create SharedParamCache for shared parameter caching
	sharedParamCache := cache.NewRedisSharedParamCache(
		logger,
		redisClient,
		queryClients.Shared(),
		blockSubscriber,
		cacheConfig,
	)
	if err := sharedParamCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start shared param cache: %w", err)
	}
	defer func() { _ = sharedParamCache.Close() }()
	logger.Info().Msg("shared param cache started")

	// Create SessionCache for session validation caching
	sessionCache := cache.NewRedisSessionCache(
		logger,
		redisClient,
		queryClients.Session(),
		queryClients.Shared(),
		blockSubscriber,
		cacheConfig,
	)
	if err := sessionCache.Start(ctx); err != nil {
		return fmt.Errorf("failed to start session cache: %w", err)
	}
	defer func() { _ = sessionCache.Close() }()
	logger.Info().Msg("session cache started")

	// Create full RelayValidator with all validations:
	// - Ring signature verification
	// - Session validity (not expired, within grace period)
	// - Supplier membership in session
	// - Application staking status (via session query)
	// HasSigner and not a snapshot of the addresses: the validator's
	// gate and the signing key have to answer the same question at the
	// same time, or a key added by a reload would be rejected here
	// while the signer holds it.
	validatorConfig := &relayer.ValidatorConfig{
		OwnsSupplierKey: responseSigner.HasSigner,
	}
	fullValidator := relayer.NewRelayValidator(
		logger,
		validatorConfig,
		ringClient,
		sessionCache,
		sharedParamCache,
	)
	proxy.SetValidator(fullValidator)
	logger.Info().
		Int("allowed_suppliers", len(responseSigner.GetOperatorAddresses())).
		Msg("full relay validator initialized with session validation")

	// Create RelayProcessor for proper relay mining with session metadata
	signerAdapter := relayer.NewResponseSignerAdapter(responseSigner)
	relayProcessor := relayer.NewRelayProcessor(
		logger,
		publisher,
		signerAdapter,
		ringClient, // Enable relay request signature verification
	)
	// Wire up the difficulty provider using on-chain service difficulty data
	difficultyProviderAdapter := &serviceDifficultyQueryAdapter{queryClient: queryClients.ServiceDifficulty()}
	difficultyProvider := relayer.NewQueryDifficultyProvider(logger, difficultyProviderAdapter)
	relayProcessor.SetDifficultyProvider(difficultyProvider)

	// Wire the service compute units provider to the height-aware query client:
	// the mined ComputeUnitsPerRelay becomes the SMST leaf weight, and the chain
	// resolves CUPR at SESSION START when validating the claim and settling it.
	// The orchestrator-refreshed serviceCache stays wired as the fallback for
	// relays that carry no session start height.
	// Shared with the relay meter below: the meter MUST price a relay with the
	// same compute units the relay is mined with, or the supplier is billed
	// against one number and paid against another.
	computeUnitsProvider := relayer.NewServiceCacheComputeUnitsProvider(logger, serviceCache, queryClients.Service())
	relayProcessor.SetServiceComputeUnitsProvider(computeUnitsProvider)

	// NOTE: App discovery callbacks are no longer needed on the relayer.
	// Discovery happens on the miner side: the supplier worker SAdds apps/
	// services seen in Redis-stream relays to the shared known-sets, which the
	// leader's CacheOrchestrator refreshes. Relayers only consume shared caches.

	proxy.SetRelayProcessor(relayProcessor)
	logger.Info().Msg("relay processor initialized")

	// Create and wire the relay meter. It is not optional: a relay the meter cannot
	// charge is a relay served for free.
	relayMeterConfig := relayer.RelayMeterConfig{
		CacheTTL: config.RelayMeter.CacheTTL,
	}

	// Create service factor client for reading service factors from Redis
	// Service factors are published by the miner
	serviceFactorClient := relayer.NewServiceFactorClient(
		logger,
		redisClient,
	)
	if err := serviceFactorClient.Start(ctx); err != nil {
		return fmt.Errorf("failed to start service factor client: %w", err)
	}
	defer func() { _ = serviceFactorClient.Close() }()

	// Use the cached application client so app stake changes
	// land within RefreshIntervalBlocks via the orchestrator's
	// invalidation pub/sub, and the hot path avoids chain
	// round-trips on every relay. GetApplication resolves through
	// the entity cache; GetParams is routed to the query-layer app
	// client (90s TTL) so the meter's app_min_stake_upokt reflects
	// the on-chain application MinStake instead of a frozen 0 — the
	// plain cached client stubs GetParams to (nil, nil).
	relayMeter := relayer.NewRelayMeter(
		logger,
		redisClient,
		cache.NewCachedApplicationQueryClientWithParams(applicationCache, queryClients.Application()),
		queryClients.Shared(),
		queryClients.Session(),
		blockSubscriber,
		sharedParamCache,    // L1->L2->L3 cache for shared params (no Redis blocking!)
		serviceCache,        // L1->L2->L3 cache for service data (no Redis blocking!)
		serviceFactorClient, // Reads service factors from Redis (published by miner)
		relayMeterConfig,
	)

	// Price relays at the session-start CUPR, matching what the relay
	// processor stamps into the SMST and what the chain settles against.
	relayMeter.SetServiceComputeUnitsProvider(computeUnitsProvider)

	if err := relayMeter.Start(ctx); err != nil {
		return fmt.Errorf("failed to start relay meter: %w", err)
	}
	defer func() { _ = relayMeter.Close() }()

	proxy.SetRelayMeter(relayMeter)
	// Served relays are charged by the batch dispatcher, and admission closes when
	// that dispatcher stops reaching Redis. Both come from the concrete batcher,
	// and until they are wired the meter refuses every relay.
	batcher.SetChargeLedger(relayMeter.ChargeLedger())
	relayMeter.SetDispatcherHeartbeat(batcher.LastSuccess)
	logger.Info().
		Msg("relay meter initialized and wired")

	// Fill the meter's view with the pairs this replica already meters, so the
	// first relay of each after a restart does not wait on Redis. Speed only: a
	// pair it misses is read on its first admission, and a failure here does not
	// stop the relayer, whose admission stays fail-closed without Redis.
	if config.CacheWarmup.Enabled {
		const meterWarmupTimeout = 10 * time.Second
		warmCtx, cancelWarm := context.WithTimeout(ctx, meterWarmupTimeout)
		warmed, warmErr := relayMeter.WarmFromRedis(warmCtx, responseSigner.HasSigner)
		cancelWarm()
		if warmErr != nil {
			logger.Warn().Err(warmErr).Int("warmed_pairs", warmed).
				Msg("relay meter warmup incomplete (continuing)")
		} else {
			logger.Info().Int("warmed_pairs", warmed).Msg("relay meter warmed from redis")
		}
	}

	// Initialize unified relay pipeline (validation + metering + signing + publishing).
	// BEFORE the gRPC handler, which copies the pipeline when it is built.
	if err := proxy.InitializeRelayPipeline(); err != nil {
		return fmt.Errorf("failed to initialize relay pipeline: %w", err)
	}
	// Initialize gRPC handler for gRPC and gRPC-Web requests
	if err := proxy.InitGRPCHandler(); err != nil {
		return fmt.Errorf("failed to initialize gRPC handler: %w", err)
	}

	// Set supplier cache for checking supplier state before accepting relays
	proxy.SetSupplierCache(supplierCache)

	// Register backend pools for health checking (per RPC type)
	for serviceID, svc := range config.Services {
		for rpcType, backend := range svc.Backends {
			if backend.HealthCheck != nil && backend.HealthCheck.Enabled {
				poolKey := fmt.Sprintf("%s:%s", serviceID, rpcType)
				p := config.GetPool(serviceID, rpcType)
				if p == nil {
					logger.Warn().
						Str("service_id", serviceID).
						Str("rpc_type", rpcType).
						Msg("health check enabled but no pool found, skipping")
					continue
				}
				healthChecker.RegisterPool(poolKey, p.All(), backend.HealthCheck, backend.Headers, backend.Authentication, backend.BasePath)
			}
		}
	}

	// Start health/readiness server
	if config.HealthCheck.Enabled {
		healthServer := startHealthServer(ctx, logger, config.HealthCheck.Addr, supplierCache, config, proxy)
		defer func() { _ = healthServer.Close() }()
		logger.Info().Str("addr", config.HealthCheck.Addr).Msg("health/readiness server started")
	}

	// Start components
	if err := healthChecker.Start(ctx); err != nil {
		return fmt.Errorf("failed to start health checker: %w", err)
	}

	if err := proxy.Start(ctx); err != nil {
		return fmt.Errorf("failed to start proxy: %w", err)
	}

	logger.Info().
		Str("listen_addr", config.ListenAddr).
		Int("num_services", len(config.Services)).
		Msg("HA Relayer started")

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info().Msg("shutdown signal received, stopping HA Relayer...")

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	// The 30s budget above is the deadline the drain actually runs under. It used
	// to be built and thrown away with a comment saying it was "used for graceful
	// shutdown timing", while the real deadline was a second, hardcoded 30s
	// inside the proxy that nothing could reach.
	_ = proxy.Close(shutdownCtx)
	_ = healthChecker.Close()

	logger.Info().Msg("HA Relayer stopped")
	return nil
}

// startHealthServer starts a simple HTTP server for health and readiness checks.
// The /ready/{service} route delegates to proxy.ServeReadyService so the
// JSON shape stays identical whether operators hit the health port or the
// main relay port.
func startHealthServer(
	ctx context.Context,
	logger logging.Logger,
	addr string,
	supplierCache *cache.SupplierCache,
	config *relayer.Config,
	proxy *relayer.ProxyServer,
) *http.Server {
	mux := http.NewServeMux()

	// /health - liveness probe (always returns OK if server is running)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK")) //nolint:errcheck // the status code already went out (WriteHeader above), so a failed body write means the client is gone: nothing left to act on
	})

	// /ready - readiness probe (checks if supplier cache has data)
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if supplierCache == nil {
			http.Error(w, "supplier cache not initialized", http.StatusServiceUnavailable)
			return
		}
		// An unpriced relayer refuses every relay it is sent, so reporting it
		// ready would route traffic it can only reject. It clears by itself
		// once the miner publishes the service factor manifest.
		if !proxy.Priced() {
			http.Error(w, "no service factor manifest: the miner has not published one yet", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("READY")) //nolint:errcheck // the status code already went out (WriteHeader above), so a failed body write means the client is gone: nothing left to act on
	})

	// /ready/{service} - per-service readiness with pool + backend state.
	// Implementation lives in the relayer package (proxy.ServeReadyService)
	// so it can read the in-flight gauge and resolve pool profiles without
	// duplicating logic here.
	mux.HandleFunc("/ready/", func(w http.ResponseWriter, r *http.Request) {
		serviceID := strings.TrimPrefix(r.URL.Path, "/ready/")
		proxy.ServeReadyService(w, serviceID)
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("health server error")
		}
	}()

	// Shutdown on context cancellation
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	return server
}

// verifyRPCConnectivity checks HTTP RPC endpoint health via /status endpoint.
// This is a non-blocking health check - failure is logged but doesn't prevent startup.
func verifyRPCConnectivity(ctx context.Context, logger logging.Logger, rpcURL string) error {
	// Create HTTP client with short timeout for health check
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Convert tcp:// scheme to http:// for HTTP client
	// CometBFT RPC URLs often use tcp:// prefix but serve HTTP
	httpURL := rpcURL
	if strings.HasPrefix(rpcURL, "tcp://") {
		httpURL = "http://" + strings.TrimPrefix(rpcURL, "tcp://")
	}

	// Query /status endpoint
	statusURL := strings.TrimSuffix(httpURL, "/") + "/status"
	req, err := http.NewRequestWithContext(ctx, "GET", statusURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	logger.Debug().
		Str("rpc_url", httpURL).
		Int("status_code", resp.StatusCode).
		Msg("RPC /status endpoint responded successfully")

	return nil
}

// verifyGRPCConnectivity checks gRPC endpoint health by querying shared params.
// This is a non-blocking health check - failure is logged but doesn't prevent startup.
func verifyGRPCConnectivity(ctx context.Context, logger logging.Logger, grpcURL string, queryClients *query.Clients) error {
	// Create context with short timeout for health check
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// Try to query shared params as a connectivity test
	_, err := queryClients.Shared().GetParams(checkCtx)
	if err != nil {
		return fmt.Errorf("gRPC query failed: %w", err)
	}

	logger.Debug().
		Str("grpc_url", grpcURL).
		Msg("gRPC endpoint responded successfully to GetParams query")

	return nil
}
