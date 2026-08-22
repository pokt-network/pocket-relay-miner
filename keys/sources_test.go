//go:build test

package keys

import (
	"io"
	"os"
	"path/filepath"
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

// TestValidatePassphraseSourceAllowsStdinAndRefusesTheContradictions states what
// a passphrase configuration may say. It may say nothing -- piping the passphrase
// in is a real way to run this -- but it may not say two things at once, nor
// configure a passphrase for a backend that has none.
func TestValidatePassphraseSourceAllowsStdinAndRefusesTheContradictions(t *testing.T) {
	t.Run("file backend with a file", func(t *testing.T) {
		require.NoError(t, ValidatePassphraseSource("file", PassphraseSource{File: "/secrets/pp"}))
	})

	t.Run("file backend with an env var name", func(t *testing.T) {
		require.NoError(t, ValidatePassphraseSource("file", PassphraseSource{Env: "KEYRING_PASSPHRASE"}))
	})

	t.Run("file backend with neither is allowed: that is stdin", func(t *testing.T) {
		require.NoError(t, ValidatePassphraseSource("file", PassphraseSource{}),
			"refusing this would outlaw `echo \"$SECRET\" | pocket-relay-miner ...`")
	})

	t.Run("both at once is refused", func(t *testing.T) {
		err := ValidatePassphraseSource("file", PassphraseSource{File: "/secrets/pp", Env: "KEYRING_PASSPHRASE"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("a passphrase for a backend that takes none is refused", func(t *testing.T) {
		err := ValidatePassphraseSource("test", PassphraseSource{File: "/secrets/pp"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "takes no passphrase")
	})
}

// TestPassphraseReaderYieldsTheSecretTwiceWithoutTrailingNewlines covers the two
// details that decide whether an unlock works at all: cosmos-sdk reads a LINE, so
// a secret file saved without a trailing newline must still produce one, and it
// asks a SECOND time when it is creating the keyring.
func TestPassphraseReaderYieldsTheSecretTwiceWithoutTrailingNewlines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "passphrase")
	// Written WITH a trailing newline, as most secret tooling does; the reader
	// must not pass it through as an empty second line.
	require.NoError(t, os.WriteFile(path, []byte("localnet-passphrase\n"), 0o600))

	r, err := PassphraseReader(PassphraseSource{File: path})
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, "localnet-passphrase\nlocalnet-passphrase\n", string(got))

	t.Run("no trailing newline in the file works too", func(t *testing.T) {
		bare := filepath.Join(dir, "bare")
		require.NoError(t, os.WriteFile(bare, []byte("no-newline-here"), 0o600))
		r, err := PassphraseReader(PassphraseSource{File: bare})
		require.NoError(t, err)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, "no-newline-here\nno-newline-here\n", string(got))
	})

	t.Run("env var, same shape", func(t *testing.T) {
		t.Setenv("PRM_TEST_PASSPHRASE", "from-the-environment")
		r, err := PassphraseReader(PassphraseSource{Env: "PRM_TEST_PASSPHRASE"})
		require.NoError(t, err)
		got, err := io.ReadAll(r)
		require.NoError(t, err)
		require.Equal(t, "from-the-environment\nfrom-the-environment\n", string(got))
	})

	t.Run("an env var that is not set is an error, not an empty passphrase", func(t *testing.T) {
		_, err := PassphraseReader(PassphraseSource{Env: "PRM_TEST_PASSPHRASE_MISSING"})
		require.Error(t, err)
		require.Contains(t, err.Error(), "PRM_TEST_PASSPHRASE_MISSING")
	})

	t.Run("nothing configured means stdin", func(t *testing.T) {
		r, err := PassphraseReader(PassphraseSource{})
		require.NoError(t, err)
		require.Nil(t, r, "nil is the signal to fall back to stdin")
	})
}
