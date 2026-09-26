package cmd

import (
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// helpRoots builds the commands main.go registers, once per process: RelayCmd
// adds its flags to a package-level command, so building it twice panics.
var helpRoots = sync.OnceValue(func() []*cobra.Command {
	return []*cobra.Command{RelayerCmd(), MinerCmd(), RedisCmd(), RelayCmd(), VersionCmd()}
})

// Every example in the help text is a command an operator can paste, so it runs
// this binary. Two of them showed `pocketd relayminer ha ...`, a binary that is
// not this one. The commands walked are the ones main.go registers.
func TestHelpExamplesRunThisBinary(t *testing.T) {
	roots := helpRoots()

	var checked int
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, line := range exampleLines(c.Long) {
			checked++
			require.True(t, strings.HasPrefix(line, "pocket-relay-miner "),
				"%q: example %q does not run pocket-relay-miner", c.CommandPath(), line)
		}
		for _, line := range strings.Split(c.Example, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			checked++
			require.True(t, strings.HasPrefix(line, "pocket-relay-miner "),
				"%q: example %q does not run pocket-relay-miner", c.CommandPath(), line)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	require.GreaterOrEqual(t, checked, 4, "CONTROL: the walk must reach the relayer, miner and validate examples")
}

// exampleLines returns the indented lines that follow an "Example:" line in a
// Long text, up to the first blank or unindented line.
func exampleLines(long string) []string {
	var out []string
	inExample := false
	for _, line := range strings.Split(long, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "Example:" || trimmed == "Examples:" {
			inExample = true
			continue
		}
		if !inExample {
			continue
		}
		if trimmed == "" || !strings.HasPrefix(line, " ") {
			inExample = false
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}
