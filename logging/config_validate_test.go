package logging

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestConfigValidate pins that an invalid level or format is a load-time
// error. parseLevel used to swallow any typo by falling back to Info — an
// operator writing `level: warning` got Info and never knew.
func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "defaults are valid", cfg: DefaultConfig()},
		{name: "empty level and format are valid (defaulted)", cfg: Config{}},
		{name: "debug level", cfg: Config{Level: "debug"}},
		{name: "mixed case is accepted", cfg: Config{Level: "WARN", Format: "JSON"}},
		{
			name:    "typo'd level is an error, not silent Info",
			cfg:     Config{Level: "warning"},
			wantErr: `logging.level "warning"`,
		},
		{
			name:    "trace was removed on purpose",
			cfg:     Config{Level: "trace"},
			wantErr: `logging.level "trace"`,
		},
		{
			name:    "unknown format is an error",
			cfg:     Config{Format: "logfmt"},
			wantErr: `logging.format "logfmt"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantErr)
		})
	}
}

// TestValidate_RetiredLevelsNameTheirReplacement covers the upgrade path rather
// than the rejection. Until this validation existed an unknown level fell back
// to info in silence, so a config carrying "trace" BOOTED -- at the wrong
// verbosity. Refusing to start is the fix; refusing without naming the
// replacement turns a one-line edit into a rollback.
func TestValidate_RetiredLevelsNameTheirReplacement(t *testing.T) {
	for level, want := range map[string]string{
		"trace":    "debug",
		"warning":  "warn",
		"fatal":    "error",
		"panic":    "error",
		"disabled": "error",
	} {
		t.Run(level, func(t *testing.T) {
			err := Config{Level: level, Format: "json"}.Validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), `use "`+want+`"`,
				"the error must name the replacement, not just the valid set")
			require.Contains(t, err.Error(), "silently logged at info",
				"and say why it used to boot, so the operator knows what changed")
		})
	}

	t.Run("an unrecognisable level still gets the plain message", func(t *testing.T) {
		err := Config{Level: "verbose-ish", Format: "json"}.Validate()
		require.Error(t, err)
		require.NotContains(t, err.Error(), "use \"")
	})
}
