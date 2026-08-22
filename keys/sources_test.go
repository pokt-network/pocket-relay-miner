//go:build test

package keys

import (
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
