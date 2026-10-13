//go:build test

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/pokt-network/pocket-relay-miner/config"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/standalone/inspect"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/storage/pebblestore"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// captureStdout returns everything f writes to stdout.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	defer func() { os.Stdout = old }()
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	require.NoError(t, w.Close())
	return <-done
}

// inspectServer serves a real store holding two sessions and one supplier.
func inspectServer(t *testing.T) string {
	t.Helper()
	db, err := pebblestore.Open(zerolog.Nop(), pebblestore.Config{Path: t.TempDir(), SyncInterval: time.Hour})
	require.NoError(t, err)
	kb := redisutil.NewKeyBuilder(config.RedisNamespaceConfig{})
	store := kv.NewPebble(zerolog.Nop(), db, kb)
	broker := pebblequeue.NewBroker(zerolog.Nop(), db, store, kb.StreamPrefix())
	backend := miner.NewPebbleStoreBackend(zerolog.Nop(), db, broker, miner.SupplierManagerConfig{BlockTimeSeconds: 30})
	for id, state := range map[string]miner.SessionState{"s1": miner.SessionStateProved, "s2": miner.SessionStateClaimMissing} {
		require.NoError(t, backend.SeedSessionForTest("pokt1gate", &miner.SessionSnapshot{
			SessionID: id, SupplierOperatorAddress: "pokt1gate", ServiceID: "svc", State: state,
		}))
	}
	require.NoError(t, store.Set(context.Background(), kb.SupplierStateKey("pokt1gate"),
		[]byte(`{"status":"active","staked":true,"services":["svc"],"operator_address":"pokt1gate"}`), 0))
	require.NoError(t, store.Set(context.Background(), kb.SupplierStateKey("pokt1gone"),
		[]byte(`{"status":"unstaking","staked":false}`), 0))
	srv := httptest.NewServer(inspect.New(zerolog.Nop(), "127.0.0.1:0", inspect.Sources{
		Miner:  func() inspect.Miner { return backend },
		Queues: broker,
		KV:     store,
	}).Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = store.Close()
		_ = db.Close()
	})
	return strings.TrimPrefix(srv.URL, "http://")
}

func runInspect(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		c := standaloneInspectCmd()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		err = c.Execute()
	})
	return out, err
}

// `inspect sessions --json` is what the live gate parses with
// jq '.[] | .state', as it parses `redis sessions --json` in
// high-availability mode: an array of objects carrying the state.
func TestStandaloneInspect_SessionsJSONIsWhatTheGateParses(t *testing.T) {
	addr := inspectServer(t)
	out, err := runInspect(t, "sessions", "--supplier", "pokt1gate", "--json", "--addr", addr)
	require.NoError(t, err)
	var sessions []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &sessions), out)
	var states []string
	for _, s := range sessions {
		states = append(states, s["state"].(string))
	}
	require.ElementsMatch(t, []string{string(miner.SessionStateProved), string(miner.SessionStateClaimMissing)}, states)

	out, err = runInspect(t, "sessions", "--supplier", "pokt1nobody", "--json", "--addr", addr)
	require.NoError(t, err)
	require.JSONEq(t, "[]", out, "no sessions is an empty array")
}

// `inspect supplier --list` prints the rows the live gate's awk picks the
// active suppliers from, as `redis supplier --list` does.
func TestStandaloneInspect_SupplierListIsWhatTheGateParses(t *testing.T) {
	addr := inspectServer(t)
	out, err := runInspect(t, "supplier", "--list", "--addr", addr)
	require.NoError(t, err)
	var active []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && strings.HasPrefix(f[0], "pokt") && f[1] == "active" {
			active = append(active, f[0])
		}
	}
	require.Equal(t, []string{"pokt1gate"}, active, out)
}

// A server that does not answer is an error, which the gate turns into a
// failure: never an empty list read as "no failed sessions".
func TestStandaloneInspect_NoServerIsAnError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	_, err = runInspect(t, "sessions", "--supplier", "pokt1gate", "--json", "--addr", addr, "--timeout", "2s")
	require.Error(t, err)
	require.Contains(t, err.Error(), "inspect server at "+addr)
}
