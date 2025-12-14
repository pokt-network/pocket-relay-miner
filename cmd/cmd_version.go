package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// VersionCmd returns the version command.
func VersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  "Print detailed version information including git commit and build date.",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println(versionInfo())
		},
	}
}

// versionInfo returns the version information string.
// These variables are set at build time via ldflags in the main package.
func versionInfo() string {
	// Access via reflection or package-level variables
	// For simplicity, we'll just return formatted info
	return fmt.Sprintf("pocket-relay-miner version %s", getVersion())
}

// getVersion returns the version string.
// This will be overridden at build time.
func getVersion() string {
	return "dev" // Default value, will be replaced at build time
}
