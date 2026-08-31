package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Build metadata, injected by main via SetVersionInfo. The Makefile's ldflags
// stamp main.Version/main.Commit/main.BuildDate (see LDFLAGS in Makefile);
// main passes them down here before executing the root command. The defaults
// only survive a bare `go build`, which the Makefile exists to prevent.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

// SetVersionInfo receives the ldflags-stamped build metadata from package main.
func SetVersionInfo(version, commit, date string) {
	if version != "" {
		buildVersion = version
	}
	if commit != "" {
		buildCommit = commit
	}
	if date != "" {
		buildDate = date
	}
}

// VersionCmd returns the version command.
func VersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Long:  "Print detailed version information including git commit and build date.",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("pocket-relay-miner version %s (commit %s, built %s)\n",
				buildVersion, buildCommit, buildDate)
		},
	}
}
