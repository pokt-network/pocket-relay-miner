//go:build test

package keys

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestValidateKeySourcesDemandsExactlyOne pins the rule both binaries share.
//
// The two-sources case is the one worth having a test for: it is a deliberate
// refusal, not a missing feature, so a later reader who finds the manager
// happily merging providers does not "fix" the validation by allowing it.
func TestValidateKeySourcesDemandsExactlyOne(t *testing.T) {
	tests := []struct {
		name           string
		keysFile       string
		keyringBackend string
		wantErr        bool
		errMentions    []string
	}{
		{
			name:     "only a keys file",
			keysFile: "/keys/supplier-keys.yaml",
		},
		{
			name:           "only a keyring",
			keyringBackend: "test",
		},
		{
			name:           "both is refused, naming both fields",
			keysFile:       "/keys/supplier-keys.yaml",
			keyringBackend: "test",
			wantErr:        true,
			errMentions:    []string{"mutually exclusive", "keys_file", "keyring", "/keys/supplier-keys.yaml", "test"},
		},
		{
			name:        "neither is refused",
			wantErr:     true,
			errMentions: []string{"no key source configured", "keys_file", "keyring"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKeySources(tt.keysFile, tt.keyringBackend)
			if !tt.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.errMentions {
				require.True(t, strings.Contains(err.Error(), want),
					"the error must name %q so an operator knows what to change; got: %s", want, err)
			}
		})
	}
}

// TestNoKeysLoadedErrorNamesTheSourceAndTheUID pins what this error has to say.
//
// The wording it replaced ("check your key file configuration") sent the reader
// looking for a mistake in the config when the config was right and the process
// simply could not read the files -- which is what actually happened, live, with
// a keyring written by a root init container and read by a process running as
// uid 1000. Naming the source and the uid is the whole value.
func TestNoKeysLoadedErrorNamesTheSourceAndTheUID(t *testing.T) {
	uid := strconv.Itoa(os.Getuid())

	t.Run("keys file", func(t *testing.T) {
		err := NoKeysLoadedError("/keys/supplier-keys.yaml", "", "")
		require.Contains(t, err.Error(), "/keys/supplier-keys.yaml")
		require.Contains(t, err.Error(), uid)
		require.NotContains(t, err.Error(), "keyring", "a keys_file fault must not talk about a keyring")
	})

	t.Run("keyring wins when both are somehow set", func(t *testing.T) {
		// Config validation refuses both, so this only fixes which one the
		// message describes if it ever gets here.
		err := NoKeysLoadedError("/keys/supplier-keys.yaml", "test", "/keyring")
		require.Contains(t, err.Error(), "test")
		require.Contains(t, err.Error(), "/keyring")
		require.Contains(t, err.Error(), uid)
	})
}

// TestValidateKeyringBackendAllowsOnlyWhatIsConfirmed states which backends this
// project supports, and -- more usefully -- why each rejection is a rejection, so
// the next reader does not widen the list back out for symmetry with cosmos-sdk.
func TestValidateKeyringBackendAllowsOnlyWhatIsConfirmed(t *testing.T) {
	for _, backend := range []string{"file", "test"} {
		t.Run("allows "+backend, func(t *testing.T) {
			require.NoError(t, ValidateKeyringBackend(backend))
		})
	}

	t.Run("memory is refused because it can never hold a key", func(t *testing.T) {
		err := ValidateKeyringBackend("memory")
		require.Error(t, err)
		require.Contains(t, err.Error(), "starts empty")
		require.Contains(t, err.Error(), "file")
	})

	t.Run("os is refused because its behaviour depends on the host", func(t *testing.T) {
		err := ValidateKeyringBackend("os")
		require.Error(t, err)
		require.Contains(t, err.Error(), "system keychain")
		require.Contains(t, err.Error(), "file", "the error must point at the backend to use instead")
	})

	t.Run("an unknown backend lists the valid ones", func(t *testing.T) {
		err := ValidateKeyringBackend("kwallet")
		require.Error(t, err)
		require.Contains(t, err.Error(), "file")
		require.Contains(t, err.Error(), "test")
	})
}

// TestOneBackendListForEveryCaller pins that there is no second, looser list
// somewhere. The CLI used to accept "os" while a service refused it, which only
// taught an operator that a key they had just resolved was usable.
func TestOneBackendListForEveryCaller(t *testing.T) {
	require.Equal(t, []string{"file", "test"}, keyringBackends,
		"widening this list means revisiting the CLI flag help, both schemas and DIRECT_CLI.md together")
}
