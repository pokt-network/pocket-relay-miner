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
		Short: "Pocket Network High-Availability RelayMiner",
		Long: `High-Availability (HA) RelayMiner for Pocket Network Shannon.

The HA RelayMiner enables running multiple RelayMiner instances behind a load balancer
with shared state via Redis. This provides:

- Horizontal scaling for high throughput
- Automatic failover for high availability
- Shared session state across instances
- Redis Streams for relay message coordination`,
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
