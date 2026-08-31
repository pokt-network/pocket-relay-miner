//go:build test

package relayer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/pool"
)

// schemePromiseRe extracts the scheme list the operator-facing artefacts
// advertise for a backend url, e.g. "Supports http://, https://, ws://, ...".
var schemePromiseRe = regexp.MustCompile(`Supports ((?:[a-z]+://,?\s*)+)`)

// promisedSchemes reads the scheme list out of an artefact the operator reads.
// It returns nil when the file carries no promise, so a caller can fail with a
// message naming the file instead of silently testing nothing.
func promisedSchemes(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "the promise artefact must exist")

	m := schemePromiseRe.FindSubmatch(data)
	if m == nil {
		return nil
	}
	var out []string
	for _, tok := range strings.Split(string(m[1]), ",") {
		if s := strings.TrimSuffix(strings.TrimSpace(tok), "://"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TestBackendURLSchemePromiseIsHonoured reads the promise out of the two
// artefacts an operator actually reads and proves the code honours every scheme
// in it. It deliberately does NOT restate the list: a test carrying its own copy
// drifts exactly the way the code drifted.
//
// The divergence this closes is measured. config.relayer.schema.yaml and the
// BackendConfig doc have advertised grpc:// and grpcs:// since the initial
// commit (5d87d33, 2025-12-07). The dialer stopped accepting them on 2026-07-13
// (e8348c4) without touching either artefact, and the promise was even
// REAFFIRMED four days later (8dc21d9). Seven months of `relayer validate`
// passing on a config that died on the first relay.
func TestBackendURLSchemePromiseIsHonoured(t *testing.T) {
	// Each promised scheme must be dialable for at least one backend type --
	// the promise is a union over types, not a per-type list.
	typesFor := map[string][]string{
		"http":  {BackendTypeJSONRPC, BackendTypeREST, BackendTypeCometBFT, BackendTypeGRPC},
		"https": {BackendTypeJSONRPC, BackendTypeREST, BackendTypeCometBFT, BackendTypeGRPC},
		"ws":    {BackendTypeWebSocket},
		"wss":   {BackendTypeWebSocket},
		"grpc":  {BackendTypeGRPC},
		"grpcs": {BackendTypeGRPC},
	}

	for _, artefact := range []string{
		filepath.Join("..", "config.relayer.schema.yaml"),
		"config.go",
	} {
		t.Run(artefact, func(t *testing.T) {
			schemes := promisedSchemes(t, artefact)
			require.NotEmpty(t, schemes,
				"%s no longer states which schemes a backend url supports; "+
					"either restore the promise or delete this artefact from the test", artefact)

			for _, scheme := range schemes {
				types, known := typesFor[scheme]
				require.True(t, known,
					"%s promises %q:// but this test does not know which backend type "+
						"should accept it -- a scheme was added to the promise without "+
						"deciding what honours it", artefact, scheme)

				accepted := false
				for _, rpcType := range types {
					if validateBackendURLScheme("svc", rpcType, scheme+"://node:9090") == nil {
						accepted = true
						break
					}
				}
				require.True(t, accepted,
					"%s promises %q:// but validateBackendURLScheme rejects it for every "+
						"type that should accept it (%v)", artefact, scheme, types)
			}
		})
	}
}

// TestValidateGRPCSchemeMatchesTheDialer is the coupling test, and it runs in
// BOTH directions. One direction alone is how the two definitions drifted:
// accepted-but-undialable is the bug that shipped, and dialable-but-rejected is
// the same bug with the sides swapped -- validation quietly stricter than the
// dialer, with nothing to notice it.
func TestValidateGRPCSchemeMatchesTheDialer(t *testing.T) {
	candidates := []string{
		"node:9090", "http://node:9090", "https://node:443",
		"grpc://node:9090", "grpcs://node:443",
		"ws://node:9090", "wss://node:9090", "ftp://node:9090",
	}

	for _, raw := range candidates {
		t.Run(raw, func(t *testing.T) {
			validateAccepts := validateBackendURLScheme("svc", BackendTypeGRPC, raw) == nil

			ep, err := pool.NewBackendEndpoint("", raw)
			dialerAccepts := err == nil &&
				(ep.URL.Scheme == "http" || ep.URL.Scheme == "https")

			require.Equal(t, dialerAccepts, validateAccepts,
				"validate and the dialer disagree about %q: validate accepts=%v, "+
					"dialer produces a dialable URL=%v", raw, validateAccepts, dialerAccepts)
		})
	}
}

// TestValidateBackendURLScheme_WebSocketUnaffected guards the pre-existing
// websocket rule against the gRPC branch.
func TestValidateBackendURLScheme_WebSocketUnaffected(t *testing.T) {
	require.NoError(t, validateBackendURLScheme("svc", BackendTypeWebSocket, "ws://b:1"))
	require.NoError(t, validateBackendURLScheme("svc", BackendTypeWebSocket, "wss://b:1"))
	require.Error(t, validateBackendURLScheme("svc", BackendTypeWebSocket, "http://b:1"))
	require.NoError(t, validateBackendURLScheme("svc", BackendTypeJSONRPC, "grpc://b:1"),
		"non-websocket, non-grpc types are not scheme-constrained")
}
