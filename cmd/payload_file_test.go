//go:build test

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/cmd/relay"
)

// withPayload sets the package-level flag value and restores it, so these tests
// can run in any order without leaking into each other.
func withPayload(t *testing.T, value string) {
	t.Helper()
	previous := relay.RelayPayloadJSON
	relay.RelayPayloadJSON = value
	t.Cleanup(func() { relay.RelayPayloadJSON = previous })
}

// TestAPayloadFileIsReadAsTheBodyAndNotAsItsPath is the case the whole change
// exists for: a body too big to travel as a command-line argument.
//
// The assertion is on the CONTENT. "It did not error" would pass while the
// relay carried the literal "@/tmp/whatever.json", which is exactly the silent
// failure this replaces -- the backend answers a JSON error naming the payload,
// and nothing names the resolution that never happened.
func TestAPayloadFileIsReadAsTheBodyAndNotAsItsPath(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_getLogs","params":[{"fromBlock":"0x0"}]}`
	path := filepath.Join(t.TempDir(), "payload.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	withPayload(t, "@"+path)
	require.NoError(t, resolvePayloadFile())

	require.Equal(t, body, relay.RelayPayloadJSON,
		"the payload must be the file's CONTENT; carrying the path means every relay sends a literal @path")
	require.NotContains(t, relay.RelayPayloadJSON, "@",
		"nothing of the marker may survive into the body")
}

// TestAPayloadWithoutTheMarkerIsLeftAlone pins that the ordinary form still
// works: the marker is opt-in and everything else is a body.
func TestAPayloadWithoutTheMarkerIsLeftAlone(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}`
	withPayload(t, body)
	require.NoError(t, resolvePayloadFile())
	require.Equal(t, body, relay.RelayPayloadJSON)

	withPayload(t, "")
	require.NoError(t, resolvePayloadFile())
	require.Equal(t, "", relay.RelayPayloadJSON, "an empty flag stays empty: the caller uses its built-in payload")
}

// TestAnUnreadablePayloadFileFailsAndNamesThePath guards the direction that
// actually costs a run.
//
// Falling back to the default payload here would make "the file was missing"
// and "the queue never filled" look the same afterwards -- a run that proves
// nothing and reports success.
func TestAnUnreadablePayloadFileFailsAndNamesThePath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here.json")
	withPayload(t, "@"+missing)

	err := resolvePayloadFile()
	require.Error(t, err, "a payload file that cannot be read must stop the run, not fall back silently")
	require.Contains(t, err.Error(), missing, "the error must name the path the operator typed")
	require.Equal(t, "@"+missing, relay.RelayPayloadJSON, "nothing was resolved, so nothing was replaced")

	// The marker with no path at all is the same class of mistake.
	withPayload(t, "@")
	require.Error(t, resolvePayloadFile(), "@ with no path is not a body named \"@\"")
}

// TestAnEmptyPayloadFileIsRefused covers the file that exists and says nothing.
// It would otherwise send an empty body and read as "the service answered
// badly" rather than "you sent nothing".
func TestAnEmptyPayloadFileIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	withPayload(t, "@"+path)
	err := resolvePayloadFile()
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "empty")
}
