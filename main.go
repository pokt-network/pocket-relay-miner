package main

import (
	"os"

	"github.com/spf13/cobra"
	_ "go.uber.org/automaxprocs" // Automatically set GOMAXPROCS based on cgroup limits

	"github.com/pokt-network/pocket-relay-miner/cmd"
)

func main() {
	// Version/Commit/BuildDate live in version.go, stamped by the Makefile's
	// ldflags; hand them to the cmd package so `version` prints the real
	// build instead of its compiled-in "dev" defaults.
	cmd.SetVersionInfo(Version, Commit, BuildDate)
	rootCmd := &cobra.Command{
		Use:   "pocket-relay-miner",
		Short: "Pocket Network RelayMiner",
		Long: `Relay miner for Pocket Network Shannon.

A deployment is one relayer, one miner and one Redis. The relayer validates,
charges and serves relays and publishes them to Redis Streams; the miner builds
the claim trees from those streams and submits claims and proofs. All session
state lives in Redis.

Start with AGENTS.md or docs/deploy/README.md in the repository.`,
	}

	// Add relayer and miner subcommands directly under root
	rootCmd.AddCommand(cmd.RelayerCmd())
	rootCmd.AddCommand(cmd.MinerCmd())
	rootCmd.AddCommand(cmd.RedisCmd())
	rootCmd.AddCommand(cmd.RelayCmd())
	rootCmd.AddCommand(cmd.VersionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
