//go:build test

package miner

// Tests for the consecutive-eviction escalation path that breaks the
// evict→resume→fail loop observed on real operators upgrading from
// pre-sliding-TTL miner builds (e.g. 221bd1a) to the defensive miner
// build. Below persistentCorruptionThreshold the eviction is
// memory-only (preserving Redis for transient-failure recovery);
// at/above the threshold the manager escalates to a full session-key
// purge so the next UpdateTree starts from a fresh tree.

import (
	"fmt"
	"io"
	"net/http/httptest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/pokt-network/pocket-relay-miner/observability"
)

// TestEscalation_Metric_Wiring is kept as a standalone wiring test
// because its whole purpose IS to verify /metrics exposition — the
// same pattern used by TestMetrics_CorruptionEvictionsWiring and
// TestMetrics_PanicsRecoveredWiring already in the suite. All the
// behavior-level tests below read mgr.evictionCounts directly
// (same-package access) instead of scraping /metrics, so the metric
// is only used to test what the metric is FOR, not as a side-channel
// for internal state.

// TestEscalation_BelowThreshold_PreservesRedis asserts that the first
// N-1 consecutive evictions keep Redis state intact (the contract
// TestEvictCorruptSession_PreservesRedisState already covers for N=1;
// this widens it to every count below the threshold).
func (s *RedisSMSTTestSuite) TestEscalation_BelowThreshold_PreservesRedis() {
	supplier := "pokt1escalation_below"
	sessionID := "session_escalation_below"

	mgr := s.createTestRedisSMSTManager(supplier)
	for i := 0; i < 4; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}
	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)
	s.Require().True(s.miniRedis.Exists(nodesKey))
	s.Require().True(s.miniRedis.Exists(liveKey))

	// Fire evictions up to one below the threshold. Every one must
	// preserve Redis so the designed-for-transient resume path works.
	for i := 1; i < persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
		s.Require().Truef(s.miniRedis.Exists(nodesKey),
			"eviction #%d must NOT purge nodes hash (below threshold=%d)",
			i, persistentCorruptionThreshold)
		s.Require().Truef(s.miniRedis.Exists(liveKey),
			"eviction #%d must NOT purge live_root (below threshold=%d)",
			i, persistentCorruptionThreshold)
	}
}

// TestEscalation_AtThreshold_PurgesRedis asserts that the N-th
// consecutive eviction for the same session escalates to a Redis
// purge of all 4 session-scoped keys (claimed_root, live_root, stats,
// nodes hash). This is the exact self-heal path that breaks the
// observed production loop (Breeze: 17.6k evictions on 18 distinct
// sessions, same ones re-evicting forever because Redis state was
// poisoned but preserved).
func (s *RedisSMSTTestSuite) TestEscalation_AtThreshold_PurgesRedis() {
	supplier := "pokt1escalation_purge"
	sessionID := "session_escalation_purge"

	mgr := s.createTestRedisSMSTManager(supplier)
	for i := 0; i < 4; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}

	claimedKey := s.redisClient.KB().SMSTRootKey(supplier, sessionID)
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)
	statsKey := s.redisClient.KB().SMSTStatsKey(supplier, sessionID)
	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)

	// Seed claimed_root + stats so the purge has all 4 key types to
	// delete. claimed_root path would normally be written by FlushTree;
	// here we write directly to keep the test focused on the purge.
	s.Require().NoError(s.redisClient.Set(s.ctx, claimedKey, make([]byte, SMSTRootLen), 0).Err())
	s.Require().NoError(s.redisClient.Set(s.ctx, statsKey, "0:0", 0).Err())
	s.Require().True(s.miniRedis.Exists(liveKey))
	s.Require().True(s.miniRedis.Exists(nodesKey))

	// Drive the escalation.
	for i := 1; i <= persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
	}

	// All 4 keys must be gone.
	s.Require().Falsef(s.miniRedis.Exists(claimedKey),
		"escalation must purge claimed_root")
	s.Require().Falsef(s.miniRedis.Exists(liveKey),
		"escalation must purge live_root")
	s.Require().Falsef(s.miniRedis.Exists(statsKey),
		"escalation must purge stats")
	s.Require().Falsef(s.miniRedis.Exists(nodesKey),
		"escalation must purge nodes hash")
}

// TestEscalation_PostPurge_NextUpdateStartsFresh closes the loop: once
// the escalation path fires, a subsequent UpdateTree must build a new
// empty tree (no resume from the purged state) and process relays
// normally. This is the end-to-end evidence that the fix actually
// unwedges the session rather than just deleting data.
func (s *RedisSMSTTestSuite) TestEscalation_PostPurge_NextUpdateStartsFresh() {
	supplier := "pokt1escalation_fresh"
	sessionID := "session_escalation_fresh"

	mgr := s.createTestRedisSMSTManagerWithInterval(supplier, 1)
	for i := 0; i < 5; i++ {
		s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
			[]byte{byte(i)}, []byte{byte(i + 100)}, 10))
	}

	// Force escalation: N consecutive evictions with no successful
	// UpdateTree in between.
	for i := 1; i <= persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
	}

	// Fresh update must succeed and produce a single-leaf tree (not
	// resume the 5 pre-purge relays).
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte{byte(99)}, []byte{byte(199)}, 10))

	_, err := mgr.FlushTree(s.ctx, sessionID)
	s.Require().NoError(err)
	count, sum, err := mgr.GetTreeStats(sessionID)
	s.Require().NoError(err)
	s.Require().Equal(uint64(1), count,
		"post-purge UpdateTree must start a fresh tree (not resume the purged state)")
	s.Require().Equal(uint64(10), sum,
		"post-purge tree sum must come from the single post-purge relay only")
}

// TestEscalation_SuccessfulUpdateResetsCounter asserts that a healthy
// UpdateTree between evictions clears the counter, so a later
// transient eviction gets the full threshold budget again. Without
// this reset, long-lived sessions with any history of transient
// failures would accumulate evictions indefinitely and escalate to
// data loss on the next isolated glitch.
func (s *RedisSMSTTestSuite) TestEscalation_SuccessfulUpdateResetsCounter() {
	supplier := "pokt1escalation_reset"
	sessionID := "session_escalation_reset"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte("k0"), []byte("v0"), 10))

	// Drive (threshold-1) evictions — one shy of escalation.
	for i := 1; i < persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
	}

	// Successful UpdateTree in the middle resets the counter.
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte("k1"), []byte("v1"), 10))

	// Now drive (threshold-1) more evictions. These must stay below
	// threshold because the counter was reset by the successful update.
	// Assert every purge-candidate key survives, not just live_root — a
	// buggy purge that skipped nodes_hash or stats would escape a
	// live_root-only check.
	liveKey := s.redisClient.KB().SMSTLiveRootKey(supplier, sessionID)
	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)
	for i := 1; i < persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
		s.Require().Truef(s.miniRedis.Exists(liveKey),
			"eviction #%d after reset must not purge live_root (below threshold=%d)",
			i, persistentCorruptionThreshold)
		s.Require().Truef(s.miniRedis.Exists(nodesKey),
			"eviction #%d after reset must not purge nodes hash (below threshold=%d)",
			i, persistentCorruptionThreshold)
	}
}

// TestEscalation_FailedUpdateDoesNotResetCounter guards the invariant
// the reset call lives AFTER Commit + FlushPipeline, not after Update
// alone. A corruption shape that lets trie.Update succeed but then
// trips trie.Commit's recursive dirty-child walk (e.g. live_root that
// references nodes the Update probe did not traverse) would — if
// resetEvictionCount ran after Update — zero the counter every relay.
// The deferred evictCorruptSession would then only ever see count=1
// and never escalate, reproducing the exact infinite-loop bug this
// escalation was introduced to fix. This test exercises the full
// UpdateTree failure path and asserts the counter accumulates across
// failures and eventually purges.
func (s *RedisSMSTTestSuite) TestEscalation_FailedUpdateDoesNotResetCounter() {
	supplier := "pokt1escalation_fail_no_reset"
	sessionID := "session_escalation_fail_no_reset"

	mgr := s.createTestRedisSMSTManager(supplier)
	// Seed deeply enough that the corrupted nodes hash has many inner
	// nodes; removing them all guarantees ANY probe path hits a gap.
	s.buildTreeWithInnerNodes(mgr, sessionID, 32)

	nodesKey := s.redisClient.KB().SMSTNodesKey(supplier, sessionID)

	// Delete every inner node so the next resume sees a root that
	// references missing children — same shape as the production bug
	// (live_root bytes valid, nodes hash divergent).
	fields, err := s.redisClient.HGetAll(s.ctx, nodesKey).Result()
	s.Require().NoError(err)
	victims := make([]string, 0, len(fields))
	for field, val := range fields {
		if len(val) > 0 && val[0] == smstInnerNodePrefix {
			victims = append(victims, field)
		}
	}
	s.Require().NotEmpty(victims, "need inner nodes to corrupt")
	s.Require().NoError(s.redisClient.HDel(s.ctx, nodesKey, victims...).Err())

	// Force every UpdateTree to go through resumeTreeFromRedisLocked,
	// which will read the intact live_root and try to traverse the
	// gutted nodes hash.
	s.forceResume(mgr, sessionID)

	// Fire probes until we either observe the counter crossing 1 (proof
	// that failures accumulate and the reset does NOT zero it between
	// relays), or exhaust the probe budget. Read the in-package
	// evictionCounts field directly — same package as the code under
	// test, so no exported surface or scrape helper is needed.
	//
	// With the fix in place (reset AFTER Commit + FlushPipeline), a
	// failed probe increments the counter and the next failed probe
	// sees counter=2. With the reset running BEFORE Commit (the bug
	// this test guards against), a successful Update on any probe
	// whose path missed the corrupt subtree would zero the counter
	// before the deferred eviction increments it back to 1 — so the
	// observed maximum would stay at 1 forever.
	maxObservedCount := 0
	const probeBudget = 512
	for i := 0; i < probeBudget; i++ {
		_ = mgr.UpdateTree(s.ctx, sessionID,
			[]byte(fmt.Sprintf("probe_k%04d", i)),
			[]byte(fmt.Sprintf("probe_v%04d", i)),
			uint64(10))

		mgr.treesMu.RLock()
		c := mgr.evictionCounts[sessionID]
		mgr.treesMu.RUnlock()
		if c > maxObservedCount {
			maxObservedCount = c
		}
		// Break once we've proven accumulation past 1 — any higher
		// value rules out the reset-before-commit bug. Going all the
		// way to threshold is unnecessary and makes the test noisier.
		if maxObservedCount >= 2 {
			break
		}
		// Force resume so every probe re-enters the corrupt path
		// instead of the clean post-escalation fresh tree the purge
		// is designed to enable.
		s.forceResume(mgr, sessionID)
	}
	s.Require().GreaterOrEqualf(maxObservedCount, 2,
		"evictionCounts[%s] never exceeded 1 across %d probes — "+
			"failed UpdateTree calls are not accumulating the counter, "+
			"which means resetEvictionCount is running on a path that "+
			"does not guarantee end-to-end UpdateTree success (e.g. "+
			"reset placed before Commit or FlushPipeline)",
		sessionID, probeBudget)
}

// TestEscalation_DeleteTree_ClearsEvictionCount asserts that the full
// session-lifecycle deletion also clears the per-session corruption
// counter, preventing an unbounded accumulation of stale int entries
// in the evictionCounts map for sessions that had eviction history
// before completing their lifecycle.
func (s *RedisSMSTTestSuite) TestEscalation_DeleteTree_ClearsEvictionCount() {
	supplier := "pokt1escalation_delete_clears"
	sessionID := "session_escalation_delete_clears"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte("k"), []byte("v"), 10))

	// Accumulate one eviction (below threshold, so the entry stays
	// in the evictionCounts map).
	mgr.evictCorruptSession(s.ctx, sessionID, "update_tree_corruption")
	mgr.treesMu.RLock()
	_, present := mgr.evictionCounts[sessionID]
	mgr.treesMu.RUnlock()
	s.Require().Truef(present,
		"eviction below threshold must leave an entry in evictionCounts for subsequent accumulation")

	// Normal session lifecycle completion.
	s.Require().NoError(mgr.DeleteTree(s.ctx, sessionID))

	mgr.treesMu.RLock()
	_, presentAfter := mgr.evictionCounts[sessionID]
	mgr.treesMu.RUnlock()
	s.Require().Falsef(presentAfter,
		"DeleteTree must clear the evictionCounts entry to prevent per-session map leak across full lifecycle")
}

// TestEscalation_Metric_Wiring verifies corruption_purged_total is
// registered in MinerRegistry and incremented on escalation with the
// expected (supplier, reason) labels. Without this, operators cannot
// alert on the escalation path and will only see the upstream
// corruption_evictions_total — which fires on every eviction, making
// it impossible to distinguish "lots of transient hiccups" from
// "state permanently poisoned".
func (s *RedisSMSTTestSuite) TestEscalation_Metric_Wiring() {
	unique := fmt.Sprintf("%d", time.Now().UnixNano())
	supplier := "pokt1escalation_metric_" + unique
	sessionID := "session_escalation_metric_" + unique
	reason := "update_tree_corruption"

	mgr := s.createTestRedisSMSTManager(supplier)
	s.Require().NoError(mgr.UpdateTree(s.ctx, sessionID,
		[]byte("k"), []byte("v"), 10))

	for i := 1; i <= persistentCorruptionThreshold; i++ {
		mgr.evictCorruptSession(s.ctx, sessionID, reason)
	}

	gather := prometheus.Gatherers{observability.MinerRegistry}
	handler := promhttp.HandlerFor(gather, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	handler.ServeHTTP(rec, req)
	body, err := io.ReadAll(rec.Body)
	s.Require().NoError(err)

	// Exactly one escalation: threshold N means the N-th eviction
	// purges, so the purged counter must be 1 after N evictions
	// (regardless of threshold value).
	needle := fmt.Sprintf(
		`ha_smst_corruption_purged_total{reason=%q,supplier=%q} 1`,
		reason, supplier,
	)
	s.Require().Containsf(string(body), needle,
		"corruption_purged metric must increment exactly once per escalation\nwant: %s\ngot family:\n%s",
		needle, extractMetric(string(body), "ha_smst_corruption_purged_total"))
}
