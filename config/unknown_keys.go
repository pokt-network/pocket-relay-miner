package config

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The YAML decoder both binaries use is lenient: yaml.Unmarshal silently drops
// every key the struct does not have. That is not a defect of the library --
// yaml.v3 offers Decoder.KnownFields(true), which errors on an unknown key --
// but strict decoding was never evaluated here, in either direction. There was
// no comment, no doc and no commit that mentioned it; the repo instead grew one
// tombstone field per retired setting, each a hand-written reaction to one
// incident.
//
// Tombstones cannot cover the case that actually bites: a key that was never a
// field. config.miner.example.yaml shipped a `suppliers:` block promising
// "explicit control over which suppliers/services to handle" while no
// yaml:"suppliers" tag existed anywhere in the tree, so an operator who
// uncommented it believed they were filtering and the miner claimed for every
// key it held. Nobody buried that key because it was never removed -- it was
// never implemented.
//
// So the strict decode runs as a SECOND pass, purely as a diagnostic, and the
// two doors differ by what the caller is for:
//
//   - `validate` is strict by nature -- validating IS its job -- so an unknown
//     key there is a hard failure, no flag involved.
//   - The serving binary is friendly by default: it warns and starts, because
//     refusing to boot over a stale key would turn a rolling deploy into an
//     outage. --strict-config turns that same finding fatal for an operator who
//     wants the guarantee.
//
// Measured 2026-09-02: both example configs this repo publishes decode CLEAN
// under KnownFields(true) -- zero false positives against our own documentation
// -- and yaml.v3 penetrates the `yaml:",inline"` embedded structs, so a bad key
// inside an inline block is reported with its own type name.

// retiredKeys carries, for each setting this project removed, the sentence that
// says what its removal CHANGED for the operator.
//
// The generic "field X not found" line cannot carry that, and the difference is
// the whole value: an operator who reads "grace_period_extra_blocks not found"
// learns that a key is unknown, while the sentence below tells them their relays
// at the session edge will now be rejected instead of served for free. That
// knowledge was paid for with incidents; the struct fields that used to hold it
// were deleted because a field per retired key is config that configures nothing.
var retiredKeys = map[string]string{
	"grace_period_extra_blocks": "it extended the serve window past the chain's grace period on the admission " +
		"side only, so relays admitted in those extra blocks were served and then judged ineligible for " +
		"rewards -- served for free. Grace now follows the on-chain grace_period_end_offset_blocks exactly. " +
		"Expect relays arriving after the grace period to be rejected as expired instead of served unpaid",

	"fail_behavior": "the relayer now refuses a relay whose budget it cannot verify, and never chooses to " +
		"serve one. If this said \"open\", expect relays to be rejected during an outage of the meter's " +
		"store that were previously served unbilled",

	"redis_key_prefix": "meter keys now derive from redis.namespace. A different prefix would have moved them " +
		"mid-session, silently resetting in-flight session budgets. Do NOT point redis.namespace.base_prefix " +
		"at the retired value to preserve them: that relocates the ENTIRE keyspace, including the WAL stream " +
		"the miner consumes from. Meter keys are ephemeral and session-scoped, so a drained fleet migrates " +
		"with nothing to move",

	"hot_reload_enabled": "the setting moved to keys.hot_reload_enabled, which the miner and the relayer " +
		"share. A deployment that set it at the top level ran with key hot reload OFF while its own config " +
		"said ON, so a key added or pulled never reached the process. Set keys.hot_reload_enabled instead",

	"keys_dir": "loading supplier keys from a directory is no longer supported. Use keys.keys_file or " +
		"keys.keyring",

	"disable_claim_batching": "claims are now ALWAYS batched by session end height and there is no way to " +
		"submit them one per transaction. If this said \"true\", expect one transaction per (supplier, session " +
		"end height) instead of one per claim -- fewer transactions per window, which is what the startup " +
		"warning used to ask for. Batching is no longer a choice on either side: proofs always travel one per " +
		"transaction and claims always travel grouped",

	"disable_proof_batching": "proofs are now ALWAYS submitted one per transaction and there is no way to " +
		"group them again. A batch dies whole, so one message the chain refuses forfeited every other proof " +
		"riding with it. If this said \"false\" (the default), expect one transaction per proof instead of one " +
		"per (supplier, session end height) -- a session needing a proof is the exception, so this is a small " +
		"increase, not one transaction per session. If it said \"true\", nothing changes: that is now the only " +
		"behaviour. Claims went the other way and lost their switch too: they are ALWAYS batched by " +
		"session end height, and disable_claim_batching is retired -- see its own entry above",
}

// UnknownKeys reports every key in data that probe's type does not declare.
//
// probe must be a pointer to a zero value of the same type the caller decodes
// into; it is filled and discarded, so it never touches the caller's config.
// Taking it as an argument rather than reflecting on a type keeps this honest:
// the caller states what shape it expects, and a caller that passes the wrong
// shape gets a wrong answer loudly rather than a silent empty result.
//
// A malformed document returns nil: the caller's own lenient decode already
// failed on it and reported a better error. This function answers exactly one
// question -- which keys does the struct not have -- and stays quiet about
// everything else.
func UnknownKeys(data []byte, probe any) []string {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	err := dec.Decode(probe)
	if err == nil {
		return nil
	}

	// yaml.v3 accumulates: every unknown key lands in TypeError.Errors with its
	// line, rather than the decode stopping at the first one. That is what makes
	// this worth reporting at all -- an operator fixes the whole file in one
	// pass instead of restarting once per stale key.
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return nil
	}

	var found []string
	for _, e := range typeErr.Errors {
		// TypeError also carries type mismatches ("cannot unmarshal !!str into
		// int"), which are the lenient decode's business, not ours.
		if !strings.Contains(e, " not found in type ") {
			continue
		}
		found = append(found, describe(e))
	}

	sort.Strings(found)
	return found
}

// describe turns one yaml.v3 line into the message the operator reads, adding
// the retired-key sentence when there is one.
//
// The yaml.v3 form is: `line 42: field foo not found in type pkg.Type`.
func describe(yamlErr string) string {
	key := keyFromError(yamlErr)
	if why, retired := retiredKeys[key]; retired {
		return fmt.Sprintf("%s -- this setting was REMOVED: %s", yamlErr, why)
	}
	return yamlErr
}

// keyFromError extracts the field name from a yaml.v3 unknown-field error, or
// "" when the shape is not the one we expect. Returning "" rather than guessing
// means an unrecognised shape degrades to the plain yaml.v3 line, which is still
// correct -- it just loses the retired-key sentence.
func keyFromError(yamlErr string) string {
	const marker = "field "
	start := strings.Index(yamlErr, marker)
	if start < 0 {
		return ""
	}
	rest := yamlErr[start+len(marker):]
	end := strings.Index(rest, " not found in type ")
	if end < 0 {
		return ""
	}
	return rest[:end]
}
