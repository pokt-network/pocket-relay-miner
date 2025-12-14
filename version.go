package main

import (
	"fmt"
	"runtime"
)

// Version information. These variables are set at build time via ldflags.
var (
	// Version is the semantic version of the build (e.g., "v1.2.3")
	Version = "dev"

	// Commit is the git commit hash
	Commit = "unknown"

	// BuildDate is the date the binary was built
	BuildDate = "unknown"

	// GoVersion is the Go version used to build the binary
	GoVersion = runtime.Version()
)

// VersionInfo returns a formatted string with all version information.
func VersionInfo() string {
	return fmt.Sprintf(
		"Version:    %s\nCommit:     %s\nBuild Date: %s\nGo Version: %s",
		Version,
		Commit,
		BuildDate,
		GoVersion,
	)
}

// ShortVersion returns a compact version string.
func ShortVersion() string {
	if Commit != "unknown" && len(Commit) >= 7 {
		return fmt.Sprintf("%s-%s", Version, Commit[:7])
	}
	return Version
}
