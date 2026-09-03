package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// probeInner exists to prove the strict pass penetrates an inline embed. That
// was the point that had to be measured rather than assumed: a config whose
// unknown key sits inside a `yaml:",inline"` struct is exactly the one a
// half-working check would miss, and miner.Config embeds RedisConfig that way.
type probeInner struct {
	Port int `yaml:"port"`
}

type probeConfig struct {
	Name  string     `yaml:"name"`
	Inner probeInner `yaml:"redis"`
}

func TestUnknownKeys_CleanConfigReportsNothing(t *testing.T) {
	doc := []byte("name: ok\nredis:\n  port: 6379\n")

	require.Empty(t, UnknownKeys(doc, &probeConfig{}),
		"a config whose every key is declared must produce no diagnostic at all -- "+
			"a false positive here is worse than the silence it replaces, because it "+
			"trains the operator to ignore the warning")
}

func TestUnknownKeys_ReportsEveryKeyWithItsLine(t *testing.T) {
	// Three unknown keys, one of them nested inside the embedded struct. The
	// decoder ACCUMULATES rather than stopping at the first, which is what lets
	// an operator fix the whole file in one pass instead of restarting once per
	// stale key.
	doc := []byte("name: ok\ntotally_bogus_top: 1\nredis:\n  port: 6379\n  bogus_inline_key: 2\nanother_bogus: 3\n")

	got := UnknownKeys(doc, &probeConfig{})

	require.Len(t, got, 3, "every unknown key must be reported, not just the first: %v", got)

	joined := strings.Join(got, "\n")
	require.Contains(t, joined, "totally_bogus_top")
	require.Contains(t, joined, "another_bogus")
	require.Contains(t, joined, "bogus_inline_key",
		"a key nested in the embedded struct must be reported too")

	for _, line := range got {
		require.Contains(t, line, "line ",
			"each finding must carry its line number, or the operator has to hunt: %q", line)
	}
}

func TestUnknownKeys_ARetiredKeyCarriesWhatItsRemovalChanged(t *testing.T) {
	// This is why the tombstone STRUCT FIELDS could be deleted without losing
	// what mattered. A bare "field not found" tells the operator a key is
	// unknown; it does not tell them their relays at the session edge now get
	// rejected instead of served for free.
	doc := []byte("name: ok\ngrace_period_extra_blocks: 2\n")

	got := UnknownKeys(doc, &probeConfig{})
	require.Len(t, got, 1)

	require.Contains(t, got[0], "grace_period_extra_blocks")
	require.Contains(t, got[0], "REMOVED",
		"a retired key must be named as removed, not merely unknown")
	require.Contains(t, got[0], "served for free",
		"the sentence that says what the removal CHANGED is the whole reason this "+
			"table exists; without it the generic line would have been enough")
}

func TestUnknownKeys_EveryRetiredKeyHasASentence(t *testing.T) {
	// A retired key with an empty sentence is a tombstone that forgot its
	// epitaph: it would render as "was REMOVED: " and say nothing.
	for key, why := range retiredKeys {
		require.NotEmpty(t, why, "retired key %q carries no explanation", key)
		require.NotContains(t, key, " ", "retired key %q is not a bare YAML key", key)
	}
}

func TestUnknownKeys_AMalformedDocumentIsNotOurs(t *testing.T) {
	// The caller's own lenient decode already failed on this and reported a
	// better error. Answering "no unknown keys" here is correct: this function
	// answers one question, and a document that does not parse cannot answer it.
	doc := []byte("name: [unclosed\n")

	require.Nil(t, UnknownKeys(doc, &probeConfig{}),
		"a parse failure must not be reported as an unknown-key finding")
}

func TestUnknownKeys_ATypeMismatchIsNotAnUnknownKey(t *testing.T) {
	// TypeError carries both shapes. Reporting a mismatch here would make
	// --strict-config refuse to boot over something the lenient decode already
	// handles, which is a different decision than the one being made.
	doc := []byte("name: ok\nredis:\n  port: not-a-number\n")

	require.Empty(t, UnknownKeys(doc, &probeConfig{}),
		"a type mismatch belongs to the lenient decode, not to the unknown-key pass")
}

func TestKeyFromError_UnrecognisedShapeDegradesToThePlainLine(t *testing.T) {
	// If yaml.v3 ever changes its wording, the retired-key sentence is lost but
	// the finding is not. That is the right failure direction, and it is worth a
	// test so a future reader knows it was chosen rather than overlooked.
	require.Equal(t, "", keyFromError("some other error entirely"))
	require.Equal(t, "foo", keyFromError("line 42: field foo not found in type pkg.Type"))
}
