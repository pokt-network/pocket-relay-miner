//go:build test

package miner

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// The persisted entry's JSON tags are a CROSS-VERSION CONTRACT, and until this
// file existed nothing held them to it.
//
// Two binaries of different versions read and write the same Redis entry during
// any rolling deploy. A renamed tag is not a refactor there: the new binary
// writes `timeout_seconds` and the old one, still reading `ts`, sees an entry
// with no stored budget -- silently, because absence is a legal state with a
// documented meaning. Nothing crashes and nothing is logged; the resend simply
// stops inheriting the deadline it was supposed to inherit.
//
// Measured before writing this: six of the nine fields carry `omitempty` and
// three carry a prose contract explaining how they degrade, and NO test
// mentioned any short tag. The comments were the only thing holding the
// contract, which is the shape this repository already knows -- a comment that
// warns is the best place to look for a test that does not exist.
//
// TWO assertions are needed here and the second is not redundant, which was
// measured rather than assumed: injecting a NEW field with `omitempty` and
// leaving it unset left the golden object below completely unchanged and the
// test green. That is omitempty working as designed -- an unset field is not on
// the wire -- and it means the marshalled output cannot see a field that has
// been ADDED. So the tag SET is asserted separately, by reflection over the
// struct, where an addition is visible whether or not it is ever populated.
//
// The golden object is compared with JSONEq, which ignores key ORDER on purpose.
// Order is not part of the contract -- any conformant reader takes the keys in
// any sequence -- so pinning it would make this test fail on a harmless
// reshuffle. Names and values are the contract; the tag set is its shape.
func TestRebroadcastEntry_WireTagsAreTheContract(t *testing.T) {
	entry := rebroadcastEntry{
		MsgBytes:          []byte("msg"), // base64 "bXNn" on the wire
		SubmitHeight:      100,
		TxHash:            "LATEST",
		OrigTxHash:        "ORIG",
		ServiceID:         "svc",
		Rebroadcasts:      3,
		TimeoutSeconds:    100,
		TimeoutRegime:     "window",
		LastAttemptHeight: 7,
	}

	got, err := marshalRebroadcastEntry(entry)
	require.NoError(t, err)

	const want = `{"m":"bXNn","h":100,"t":"LATEST","o":"ORIG","s":"svc","n":3,` +
		`"ts":100,"tr":"window","l":7}`
	require.JSONEq(t, want, string(got),
		"a renamed or added tag is a BREAKING change for a mixed fleet: the "+
			"other binary keeps reading the old name and sees an absent field, "+
			"which is a legal state with a meaning of its own")

	// And the round trip is lossless for a binary that knows every field --
	// the control for the case below, which is the same trip through a binary
	// that does not.
	back, err := unmarshalRebroadcastEntry(got)
	require.NoError(t, err)
	require.Equal(t, entry, back)

	// The tag SET, which is what makes an ADDED field visible. A new field is a
	// cross-version event exactly like a renamed one -- the binary that gains it
	// writes a key the other one drops on every rewrite -- so it must not be
	// possible to add one without this test saying so and a human agreeing.
	var tags []string
	rt := reflect.TypeOf(rebroadcastEntry{})
	for i := range rt.NumField() {
		tags = append(tags, rt.Field(i).Tag.Get("json"))
	}
	require.Equal(t, []string{
		"m", "h", "t", "o,omitempty", "s,omitempty", "n,omitempty",
		"ts,omitempty", "tr,omitempty", "l,omitempty",
		"sb,omitempty", "sa,omitempty", "sh,omitempty",
	}, tags,
		"a field added to or removed from the persisted entry changes what the "+
			"other binary in a rolling deploy sees; update this list in the same "+
			"commit that changes the struct, deliberately")
}

// legacyRebroadcastEntry is the entry as a binary that predates the broadcast
// budget sees it: same tags, no `ts` and no `tr`.
//
// It is a hand-written mirror of a struct in the file under test, so it is
// exactly the kind of copy that rots. That is deliberate and it is the point:
// the test below FAILS when the mirror stops matching, because the surviving
// fields are compared one by one against the original. A silent divergence is
// what this shape usually risks; here divergence is the assertion.
type legacyRebroadcastEntry struct {
	MsgBytes          []byte `json:"m"`
	SubmitHeight      int64  `json:"h"`
	TxHash            string `json:"t"`
	OrigTxHash        string `json:"o,omitempty"`
	ServiceID         string `json:"s,omitempty"`
	Rebroadcasts      int    `json:"n,omitempty"`
	LastAttemptHeight int64  `json:"l,omitempty"`
}

// An older binary that rewrites the entry DROPS what its struct cannot see, and
// the surviving fields must come through untouched.
//
// This is the direction the field comments document and that nothing exercised.
// The other direction -- a new binary reading an entry written before the field
// existed -- is already covered behaviourally by
// TestReconciler_ResendWithoutAStoredBudgetPassesItThroughAsAbsent, which proves
// that an absent budget reaches the resubmitter AS absent instead of as an
// invented value. This test deliberately does not repeat that: it proves the
// LOSS, and that one proves what the loss costs.
//
// Why the loss is tolerable and must stay so: falling back to the chain ceiling
// under the "unknown" regime is a weaker deadline, not a lost claim, and the
// regime counter makes the fallback visible instead of hiding it. What would NOT
// be tolerable is a survivor arriving corrupted -- a resend carrying another
// session's message bytes, or an OrigTxHash that no longer keys its submission
// record -- so those are asserted field by field rather than as a whole, to name
// which one moved.
func TestRebroadcastEntry_AnOlderBinaryDropsWhatItCannotSee(t *testing.T) {
	original := rebroadcastEntry{
		MsgBytes:          []byte("msg"),
		SubmitHeight:      100,
		TxHash:            "LATEST",
		OrigTxHash:        "ORIG",
		ServiceID:         "svc",
		Rebroadcasts:      3,
		TimeoutSeconds:    100,
		TimeoutRegime:     "window",
		LastAttemptHeight: 7,
	}

	written, err := marshalRebroadcastEntry(original)
	require.NoError(t, err)

	// The old binary reads it, does its own work, and writes it back.
	var legacy legacyRebroadcastEntry
	require.NoError(t, json.Unmarshal(written, &legacy))
	rewritten, err := json.Marshal(legacy)
	require.NoError(t, err)

	// The current binary reads what the old one left.
	after, err := unmarshalRebroadcastEntry(rewritten)
	require.NoError(t, err)

	require.Zero(t, after.TimeoutSeconds,
		"the budget must be GONE, not defaulted to something plausible: the "+
			"resend has to be able to tell 'nothing was stored' from a real value")
	require.Empty(t, after.TimeoutRegime,
		"and the regime with it, so the fallback is reported as a fallback")

	require.Equal(t, original.MsgBytes, after.MsgBytes,
		"the message itself must survive a rewrite by an older binary -- losing "+
			"or corrupting it turns a recoverable session into a forfeited one")
	require.Equal(t, original.SubmitHeight, after.SubmitHeight)
	require.Equal(t, original.TxHash, after.TxHash)
	require.Equal(t, original.OrigTxHash, after.OrigTxHash,
		"and the ORIGINAL hash above all: the claim on-chain outcome is keyed by "+
			"it, so an entry that lost it stops matching its submission record")
	require.Equal(t, original.ServiceID, after.ServiceID)
	require.Equal(t, original.Rebroadcasts, after.Rebroadcasts,
		"the resend count is persisted precisely so it survives a failover; an "+
			"older binary must not reset it")

	// LastAttemptHeight is asserted as DATA only, and that is the honest limit
	// of what can be claimed about it here: it has no reader today, so there is
	// no behaviour to pin. Round-tripping is what it does, so round-tripping is
	// what is asserted -- and it is worth asserting precisely because the field
	// is invisible in behaviour, which makes it the one whose silent loss no
	// other test could catch.
	require.Equal(t, original.LastAttemptHeight, after.LastAttemptHeight,
		"a field with no reader is exactly the one that can be lost without any "+
			"behavioural test noticing")
}
