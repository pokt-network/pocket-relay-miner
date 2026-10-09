package miner

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/pebble"
)

// The Inspect methods read what the store holds for an operator, the way the
// redis subcommands read Redis in high-availability mode. They only read:
// unlike GetBySupplier, which deletes the expired sessions it walks past, an
// inspection never changes what it looks at. They take no lock either; each
// one iterates a consistent view of the store.

// SnapshotFields is a session as the fields of its Redis hash, every value a
// string: what `redis sessions --json` prints, so a consumer of that output
// reads both modes alike.
func SnapshotFields(snap *SessionSnapshot) map[string]string {
	pairs := encodeSnapshot(snap)
	out := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out[fmt.Sprint(pairs[i])] = fmt.Sprint(pairs[i+1])
	}
	return out
}

// InspectSessions returns the supplier's sessions, in state when it is not
// empty, as SnapshotFields. A session past its TTL is left out, as Redis would
// have expired its key, and left in place.
func (b *PebbleStoreBackend) InspectSessions(supplier string, state SessionState) ([]map[string]string, error) {
	prefix := []byte(pebbleSessionPrefix + supplier + "\x00")
	ttl := sessionTTL(b.config.SessionTTL)
	now := time.Now()
	out := []map[string]string{}
	err := b.iterate(prefix, func(_, value []byte) error {
		snap := &SessionSnapshot{}
		if err := json.Unmarshal(value, snap); err != nil {
			return fmt.Errorf("failed to decode a session of %s: %w", supplier, err)
		}
		if !snap.LastUpdatedAt.IsZero() && snap.LastUpdatedAt.Add(ttl).Before(now) {
			return nil
		}
		if state == "" || snap.State == state {
			out = append(out, SnapshotFields(snap))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to inspect the sessions of %s: %w", supplier, err)
	}
	return out, nil
}

// DedupView is what the store holds of one session's dedup marks.
type DedupView struct {
	SessionID string `json:"session_id"`
	// Count is the marks on disk, those past their TTL included until the sweep.
	Count int `json:"count"`
	// ExpiresAtUnixMs is when the marks stop counting, unix milliseconds as
	// stored; zero when no TTL is stored.
	ExpiresAtUnixMs int64 `json:"expires_at_unix_ms,omitempty"`
	// Live is whether the marks still count as duplicates.
	Live bool `json:"live"`
	// Sample is up to sampleSize relay hashes, hex.
	Sample []string `json:"sample"`
}

// InspectDedup returns the session's dedup marks, with up to sampleSize hashes.
func (b *PebbleStoreBackend) InspectDedup(sessionID string, sampleSize int) (DedupView, error) {
	view := DedupView{SessionID: sessionID, Sample: []string{}}
	prefix := []byte(pebbleDedupPrefix + sessionID + "\x00")
	err := b.iterate(prefix, func(key, _ []byte) error {
		view.Count++
		if len(view.Sample) < sampleSize {
			view.Sample = append(view.Sample, hex.EncodeToString(key[len(prefix):]))
		}
		return nil
	})
	if err != nil {
		return view, fmt.Errorf("failed to inspect the dedup marks of %s: %w", sessionID, err)
	}
	value, ok, err := b.get(dedupTTLKey(sessionID))
	if err != nil {
		return view, fmt.Errorf("failed to read the dedup TTL of %s: %w", sessionID, err)
	}
	if ok && len(value) == 8 {
		// Compared as stored, as ttlStateLocked does.
		view.ExpiresAtUnixMs = int64(binary.BigEndian.Uint64(value))
		view.Live = view.ExpiresAtUnixMs > time.Now().UnixMilli()
	}
	return view, nil
}

// SMSTView is what the store holds of one supplier's tree for a session.
type SMSTView struct {
	Supplier  string `json:"supplier"`
	SessionID string `json:"session_id"`
	// NodeCount is the nodes stored; zero once the tree is cold-compacted.
	NodeCount int `json:"node_count"`
	// ClaimedRoot is the sealed root the claim was built from, hex; empty
	// until the tree is sealed.
	ClaimedRoot string `json:"claimed_root,omitempty"`
	// LiveRoot is the checkpoint of a tree still taking relays, hex.
	LiveRoot string `json:"live_root,omitempty"`
	// Stats is the sealed tree's "count:sum".
	Stats string `json:"stats,omitempty"`
	// Compacted is whether the tree is kept as a leaves blob.
	Compacted bool `json:"compacted"`
}

// InspectSMST returns every supplier's tree for the session; supplier narrows
// it to one when it is not empty.
func (b *PebbleStoreBackend) InspectSMST(supplier, sessionID string) ([]SMSTView, error) {
	suppliers := []string{supplier}
	if supplier == "" {
		found, err := b.smstSuppliers(sessionID)
		if err != nil {
			return nil, err
		}
		suppliers = found
	}
	now := time.Now()
	out := []SMSTView{}
	for _, sup := range suppliers {
		s := &pebbleSMSTStore{b: b, supplier: sup}
		view := SMSTView{Supplier: sup, SessionID: sessionID}
		if err := b.iterate(s.nodesPrefix(sessionID), func(_, _ []byte) error { view.NodeCount++; return nil }); err != nil {
			return nil, fmt.Errorf("failed to count the nodes of %s for %s: %w", sessionID, sup, err)
		}
		records := []struct {
			rec smstRecord
			put func([]byte)
		}{
			{smstClaimedRoot, func(v []byte) { view.ClaimedRoot = hex.EncodeToString(v) }},
			{smstLiveRoot, func(v []byte) { view.LiveRoot = hex.EncodeToString(v) }},
			{smstStats, func(v []byte) { view.Stats = string(v) }},
			{smstLeaves, func([]byte) { view.Compacted = true }},
		}
		present := view.NodeCount > 0
		for _, r := range records {
			value, ok, err := s.readRecord(r.rec, sessionID, now)
			if err != nil {
				return nil, err
			}
			if ok {
				r.put(value)
				present = true
			}
		}
		if present {
			out = append(out, view)
		}
	}
	return out, nil
}

// smstSuppliers returns the suppliers holding a tree record for the session.
func (b *PebbleStoreBackend) smstSuppliers(sessionID string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	suffix := "\x00" + sessionID + "\x00"
	err := b.iterate([]byte(pebbleSMSTRecordPrefix), func(key, _ []byte) error {
		rest := string(key[len(pebbleSMSTRecordPrefix):])
		i := strings.Index(rest, suffix)
		if i <= 0 || len(rest) != i+len(suffix)+1 {
			return nil
		}
		if sup := rest[:i]; !seen[sup] {
			seen[sup] = true
			out = append(out, sup)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to find the trees of %s: %w", sessionID, err)
	}
	return out, nil
}

// iterate calls fn for every key under prefix, in key order.
func (b *PebbleStoreBackend) iterate(prefix []byte, fn func(key, value []byte) error) error {
	iter, err := b.store.DB().NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: keyUpperBound(prefix)})
	if err != nil {
		return err
	}
	var fnErr error
	for valid := iter.First(); valid && fnErr == nil; valid = iter.Next() {
		if !bytes.HasPrefix(iter.Key(), prefix) {
			break
		}
		fnErr = fn(iter.Key(), iter.Value())
	}
	return errors.Join(fnErr, iter.Error(), iter.Close())
}
