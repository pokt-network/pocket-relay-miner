package main

import (
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
