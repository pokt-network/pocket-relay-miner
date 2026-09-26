//go:build test

package miner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
	sharedtypes "github.com/pokt-network/poktroll/x/shared/types"
)

// nodeReadCounter counts the SMST node reads a manager sends to Redis.
type nodeReadCounter struct{ reads atomic.Int64 }

func (c *nodeReadCounter) DialHook(next goredis.DialHook) goredis.DialHook { return next }

func (c *nodeReadCounter) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		if cmd.Name() == "hget" {
			c.reads.Add(1)
		}
		return next(ctx, cmd)
	}
}

func (c *nodeReadCounter) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return next
}

// unloadParams are the mock's 4-block sessions with blocks between the end of
// the grace period and the claim window. With poktroll's defaults the two
// coincide and no unload height exists; with one block between them, as on
// localnet, no other session is still in its grace period at that height. Three
// let a test tell the two apart.
func unloadParams() *sharedtypes.Params {
	params, _ := (&mockSharedQueryClient{}).GetParams(context.Background())
	params.GracePeriodEndOffsetBlocks = 3
	params.ClaimWindowOpenOffsetBlocks = 6
	return params
}

func newUnloadManager(client *redisutil.Client, supplier string) *RedisSMSTManager {
	return NewRedisSMSTManager(zerolog.Nop(), client, RedisSMSTManagerConfig{SupplierAddress: supplier, CacheTTL: time.Hour})
}

// isUnloaded reports whether the session's tree is in the map with its trie
// imported again by UnloadTree and no relay since.
func isUnloaded(t *testing.T, m *RedisSMSTManager, sessionID string) bool {
	t.Helper()
	m.treesMu.RLock()
	tree, ok := m.trees[sessionID]
	m.treesMu.RUnlock()
	require.True(t, ok, "an unload keeps the tree in the map")
	tree.mu.Lock()
	defer tree.mu.Unlock()
	return tree.unloaded
}

func collectedHeap() uint64 {
	runtime.GC()
	return runtimeHeapLive()
}

// referenceRoot is the claimed root of the same relays in a tree nobody unloaded.
func referenceRoot(t *testing.T, ctx context.Context, client *redisutil.Client, relays []coldRelay) []byte {
	t.Helper()
	return claimColdTree(t, ctx, newUnloadManager(client, "pokt1unload_reference"), "sess-reference", relays)
}

func addRelays(t *testing.T, ctx context.Context, m *RedisSMSTManager, sessionID string, relays []coldRelay) {
	t.Helper()
	for _, r := range relays {
		require.NoError(t, m.UpdateTree(ctx, sessionID, bytes.Clone(r.key), bytes.Clone(r.value), r.weight))
	}
}

func TestUnloadTree_AClaimAndAProofOfAnUnloadedTreeReadOnlyThePathsTheyWalk(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(31, 20000)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_lazy")
	const session = "sess-lazy"
	counter := &nodeReadCounter{}
	client.AddHook(counter)
	// The heap is the whole test binary's: what the tree holds is the growth
	// from before it was built.
	before := collectedHeap()
	addRelays(t, ctx, m, session, relays)
	loaded := collectedHeap()
	readsBefore := counter.reads.Load()
	unloaded, err := m.UnloadTree(ctx, session)
	require.NoError(t, err)
	require.NotNil(t, unloaded)
	require.True(t, isUnloaded(t, m, session))
	unloadReads := counter.reads.Load() - readsBefore
	released := collectedHeap()
	again, err := m.UnloadTree(ctx, session)
	require.NoError(t, err)
	require.Nil(t, again, "LINK unload-once: a tree unloaded with no relay since is not imported again")

	readsBefore = counter.reads.Load()
	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, want, root, "the claim of the tree resumed from Redis is the claim of the tree unloaded")
	path := sha256.Sum256([]byte("unload-proof-path"))
	_, err = m.ProveClosest(ctx, session, path[:])
	require.NoError(t, err)
	walkReads := counter.reads.Load() - readsBefore
	walked := collectedHeap()
	// relays is kept live past the last reading (a len does not keep its array),
	// so the readings differ only by the tree.
	runtime.KeepAlive(relays)

	t.Logf("heap live: before the tree %d, loaded %d, unloaded %d, after claim and proof %d; node reads: unload %d, claim and proof %d, for %d leaves",
		before, loaded, released, walked, unloadReads, walkReads, len(relays))
	require.Less(t, unloadReads, int64(50),
		"LINK unload-lazy: the unload writes the tree and imports its root without reading its nodes back")
	require.Less(t, walkReads, int64(500),
		"LINK unload-lazy: a claim and a proof read the nodes on their paths, not the %d-leaf tree", len(relays))
	require.Greater(t, loaded, before, "premise: the tree grew the heap")
	require.Less(t, released-min(released, before), (loaded-before)/4,
		"LINK unload-lazy-heap: the unloaded tree holds a small part of what it added to the heap")
	require.Less(t, walked-min(walked, before), (loaded-before)/4,
		"LINK unload-lazy-heap: the tree resumed for a claim and a proof holds a small part of what it added to the heap")
}

func TestUnloadTree_ARelayAfterTheUnloadGoesIntoTheResumedTreeAndItsClaim(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(32, 200)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_grace")
	const session = "sess-grace"
	addRelays(t, ctx, m, session, relays[:199])
	unloaded, err := m.UnloadTree(ctx, session)
	require.NoError(t, err)
	require.NotNil(t, unloaded)

	addRelays(t, ctx, m, session, relays[199:])
	require.False(t, isUnloaded(t, m, session), "LINK unload-relay-clears: the late relay enters the imported trie, and the next unload drops it again")
	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, want, root, "LINK unload-grace: a relay that arrives after the unload is in the claimed root")
}

func TestUnloadTree_WritesTheRelaysAddedSinceTheLastCheckpoint(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(33, 200)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_checkpoint")
	const session = "sess-checkpoint"
	addRelays(t, ctx, m, session, relays[:100])
	_, _, err := m.CheckpointLiveRoot(ctx, session)
	require.NoError(t, err)
	addRelays(t, ctx, m, session, relays[100:])

	unloaded, err := m.UnloadTree(ctx, session)
	require.NoError(t, err)
	require.NotNil(t, unloaded)
	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, want, root)
	// The root alone is in the imported trie's stub: the nodes under it are
	// read from Redis only by a walk, so a proof is what finds them missing.
	for i := range 16 {
		path := sha256.Sum256([]byte(fmt.Sprintf("unload-checkpoint-path-%d", i)))
		proof, err := m.ProveClosest(ctx, session, path[:])
		require.NoError(t, err, "LINK unload-checkpoint: the nodes of the relays added after the last checkpoint are in Redis")
		ok, _ := chainVerifies(t, proof, root)
		require.True(t, ok, "LINK unload-checkpoint: a proof of the unloaded tree verifies against its claim")
	}
}

func TestUnloadEndedSessionTrees_UnloadsOnlyTheSessionsPastTheirGracePeriod(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	params := unloadParams()
	const ended, inGrace, claiming = 100, 104, 96
	height := sharedtypes.GetSessionGracePeriodEndHeight(params, ended) + 2
	require.LessOrEqual(t, height, sharedtypes.GetSessionGracePeriodEndHeight(params, inGrace), "premise: the second session is in its grace period")
	require.Less(t, int64(inGrace), height, "premise: the second session has ended, and is in its grace period")
	require.Greater(t, height, sharedtypes.GetSessionGracePeriodEndHeight(params, ended), "premise: the first session's grace period is over")
	require.Less(t, height, sharedtypes.GetClaimWindowOpenHeight(params, ended), "premise: the first session's claim window is not open")
	require.GreaterOrEqual(t, height, sharedtypes.GetClaimWindowOpenHeight(params, claiming), "premise: the third session's claim window is open")
	current := height + 1

	smst := newUnloadManager(client, "pokt1unload_ended")
	sessions := xsync.NewMap[string, *SessionSnapshot]()
	for id, end := range map[string]int64{"sess-ended": ended, "sess-grace": inGrace, "sess-current": current, "sess-claiming": claiming} {
		sessions.Store(id, &SessionSnapshot{SessionID: id, SessionEndHeight: end})
		addRelays(t, ctx, smst, id, coldRelays(uint64(end), 3))
	}
	m := &SupplierManager{
		logger: zerolog.Nop(),
		config: SupplierManagerConfig{BlockClient: &mockBlockClient{currentHeight: height}, SharedClient: &mockSharedQueryClient{params: params}},
	}
	state := &SupplierState{OperatorAddr: "pokt1unload_ended", SMSTManager: smst, LifecycleManager: &SessionLifecycleManager{activeSessions: sessions}}

	// A proof waiting for memory holds its tree's lock: the loop must not wait on it.
	smst.treesMu.RLock()
	claimingTree := smst.trees["sess-claiming"]
	smst.treesMu.RUnlock()
	claimingTree.mu.Lock()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		m.unloadEndedSessionTrees(ctx, state)
	}()
	select {
	case <-returned:
	case <-time.After(20 * time.Second):
		claimingTree.mu.Unlock()
		t.Fatal("LINK unload-before-claim: the unload leaves a session whose claim window is open, and does not wait on its tree")
	}
	claimingTree.mu.Unlock()
	require.False(t, isUnloaded(t, smst, "sess-claiming"), "LINK unload-before-claim: a session whose claim window is open is left to its claim")
	require.True(t, isUnloaded(t, smst, "sess-ended"), "LINK unload-ended: a session past its grace period leaves memory")
	require.False(t, isUnloaded(t, smst, "sess-grace"), "LINK unload-grace-kept: a session in its grace period stays")
	require.False(t, isUnloaded(t, smst, "sess-current"), "a session being served stays")
}

func TestUnloadTree_RacingRelaysCheckpointsAndOtherSessionsNeitherDeadlocksNorLosesARelay(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(34, 400)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_race")
	const session = "sess-race"
	addRelays(t, ctx, m, session, relays[:1])
	errs := make(chan error, 4)
	var done sync.WaitGroup
	done.Add(4)
	go func() { // relays entering the tree between unloads
		defer done.Done()
		for _, r := range relays[1:] {
			if err := m.UpdateTree(ctx, session, bytes.Clone(r.key), bytes.Clone(r.value), r.weight); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() { // the flush tick
		defer done.Done()
		for range 200 {
			if _, err := m.UnloadTree(ctx, session); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() { // the periodic checkpoint
		defer done.Done()
		for range 200 {
			if _, _, err := m.CheckpointLiveRoot(ctx, session); err != nil {
				errs <- err
				return
			}
		}
	}()
	go func() { // another session writing the tree map
		defer done.Done()
		for i, r := range coldRelays(35, 200) {
			if err := m.UpdateTree(ctx, fmt.Sprintf("sess-race-other-%d", i%8), r.key, r.value, r.weight); err != nil {
				errs <- err
				return
			}
		}
	}()
	finished := make(chan struct{})
	go func() { done.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatal("LINK unload-no-deadlock: an unload racing relays, checkpoints and other sessions finishes")
	}
	close(errs)
	for err := range errs {
		require.NoError(t, err, "LINK unload-race-keeps-relays: no racing call finds a node missing")
	}
	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, want, root, "LINK unload-race-keeps-relays: every relay is in the claimed root, whichever unload it raced")
}

func TestResetEvictionCount_DoesNotWaitOnTheTreeMap(t *testing.T) {
	client, _ := newTestRedis(t)
	m := newUnloadManager(client, "pokt1unload_eviction_lock")

	// commitLocked resets the counter holding a tree's lock, and a tree's lock is
	// taken before the map's: waiting on the map there deadlocks.
	m.treesMu.Lock()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		m.resetEvictionCount("sess-eviction-lock")
	}()
	select {
	case <-returned:
		m.treesMu.Unlock()
	case <-time.After(20 * time.Second):
		m.treesMu.Unlock()
		t.Fatal("LINK eviction-lock-own: resetting the eviction counter does not wait on the tree map")
	}
}

func TestUnloadTree_LeavesATreeBeingSealedToItsClaim(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(36, 200)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_sealing")
	const session = "sess-sealing"
	addRelays(t, ctx, m, session, relays)
	var during []byte
	var duringErr error
	installFlushSealWaitHook(func(string) {
		during, duringErr = m.UnloadTree(ctx, session)
	})
	t.Cleanup(func() { installFlushSealWaitHook(nil) })

	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.NoError(t, duringErr)
	require.Nil(t, during, "LINK unload-not-sealing: an unload while FlushTree waits for the sealed root leaves the trie to it")
	require.Equal(t, want, root)
}

func TestFlushTree_LeavesTheClaimedTreeUnloaded(t *testing.T) {
	ctx := context.Background()
	client, _ := newTestRedis(t)
	relays := coldRelays(37, 20000)
	want := referenceRoot(t, ctx, client, relays)

	m := newUnloadManager(client, "pokt1unload_claim")
	const session = "sess-claim-unload"
	// The heap is the whole test binary's: what the tree holds is the growth
	// from before it was built.
	before := collectedHeap()
	addRelays(t, ctx, m, session, relays)
	loaded := collectedHeap()
	root, err := m.FlushTree(ctx, session)
	require.NoError(t, err)
	require.Equal(t, want, root)
	claimed := collectedHeap()

	// relays is kept live past the last reading (a len does not keep its array),
	// so the readings differ only by the tree.
	runtime.KeepAlive(relays)
	t.Logf("heap live: before the tree %d, loaded %d, claimed %d; %d leaves", before, loaded, claimed, len(relays))
	require.True(t, isUnloaded(t, m, session), "LINK claim-unloads: the claim leaves the tree unloaded when no unload reached it")
	require.Greater(t, loaded, before, "premise: the tree grew the heap")
	require.Less(t, claimed-min(claimed, before), (loaded-before)/4,
		"LINK claim-unloads: the claimed tree holds a small part of what it added to the heap")
	path := sha256.Sum256([]byte("claim-unload-proof-path"))
	proof, err := m.ProveClosest(ctx, session, path[:])
	require.NoError(t, err)
	ok, _ := chainVerifies(t, proof, root)
	require.True(t, ok, "a proof of the unloaded claimed tree verifies against its claim")
}
