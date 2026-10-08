// Package inspect is the standalone process's read-only inspect server: what
// the store holds -- sessions, trees, the relay queues, meters, submissions --
// served as JSON on a loopback address, for `pocket-relay-miner standalone
// inspect` and the live gate. In high-availability mode the redis subcommands
// read Redis; here no other process can open the store while this one runs,
// so the process answers for it.
package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pokt-network/pocket-relay-miner/logging"
	"github.com/pokt-network/pocket-relay-miner/miner"
	"github.com/pokt-network/pocket-relay-miner/storage/kv"
	"github.com/pokt-network/pocket-relay-miner/transport/pebblequeue"
	redisutil "github.com/pokt-network/pocket-relay-miner/transport/redis"
)

// Routes, under PathPrefix.
const (
	PathPrefix      = "/v1/"
	PathHealth      = PathPrefix + "healthz"
	PathSessions    = PathPrefix + "sessions"
	PathSuppliers   = PathPrefix + "suppliers"
	PathStreams     = PathPrefix + "streams"
	PathSMST        = PathPrefix + "smst"
	PathDedup       = PathPrefix + "dedup"
	PathMeter       = PathPrefix + "meter"
	PathSubmissions = PathPrefix + "submissions"
)

// defaultDedupSample is how many relay hashes a dedup answer carries.
const defaultDedupSample = 10

// Miner is what the server reads of the miner's state. Every method only reads.
type Miner interface {
	InspectSessions(supplier string, state miner.SessionState) ([]map[string]string, error)
	InspectDedup(sessionID string, sampleSize int) (miner.DedupView, error)
	InspectSMST(supplier, sessionID string) ([]miner.SMSTView, error)
}

// Queues is what the server reads of the relay queues.
type Queues interface {
	Stats(supplier string) (pebblequeue.QueueStats, error)
	AllStats() ([]pebblequeue.QueueStats, error)
}

// KV is the part of the key-value store the server reads.
type KV interface {
	KB() *redisutil.KeyBuilder
	Get(ctx context.Context, key string) ([]byte, error)
	ScanPrefix(ctx context.Context, prefix string) ([]string, error)
}

// Sources are what the server reads.
type Sources struct {
	// Miner returns the miner's state once the miner has built it; until then
	// it returns nil and the miner's routes answer 503.
	Miner  func() Miner
	Queues Queues
	KV     KV
}

// MeterEntry is one supplier's meter for a session: the relayer's
// SessionMeterMeta JSON and the stake consumed so far.
type MeterEntry struct {
	Key           string          `json:"key"`
	Meta          json.RawMessage `json:"meta"`
	ConsumedUpokt *string         `json:"consumed_upokt"`
}

// Server serves the routes on a loopback address.
type Server struct {
	logger logging.Logger
	addr   string
	src    Sources
	srv    *http.Server
	once   sync.Once
}

// New returns a server for addr; Start listens.
func New(logger logging.Logger, addr string, src Sources) *Server {
	s := &Server{logger: logging.ForComponent(logger, "inspect_server"), addr: addr, src: src}
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Start listens on the address, so a port already taken is a startup error,
// and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return fmt.Errorf("inspect server: listen on %s: %w", s.addr, err)
	}
	go logging.RecoverGoRoutine(s.logger, "inspect_server", func(context.Context) {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Warn().Err(err).Msg("inspect server stopped")
		}
	})(context.Background())
	s.logger.Info().Str("addr", ln.Addr().String()).Msg("inspect server listening")
	return nil
}

// Stop shuts the server down. Idempotent.
func (s *Server) Stop() {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	})
}

// Handler is the routes, GET only.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc(PathSessions, s.sessions)
	mux.HandleFunc(PathSuppliers, s.suppliers)
	mux.HandleFunc(PathStreams, s.streams)
	mux.HandleFunc(PathSMST, s.smst)
	mux.HandleFunc(PathDedup, s.dedup)
	mux.HandleFunc(PathMeter, s.meter)
	mux.HandleFunc(PathSubmissions, s.submissions)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "read-only: GET only", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) miner(w http.ResponseWriter) Miner {
	var m Miner
	if s.src.Miner != nil {
		m = s.src.Miner()
	}
	if m == nil {
		http.Error(w, "the miner has not started yet", http.StatusServiceUnavailable)
	}
	return m
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	supplier := q.Get("supplier")
	if supplier == "" {
		http.Error(w, "supplier is required", http.StatusBadRequest)
		return
	}
	m := s.miner(w)
	if m == nil {
		return
	}
	sessions, err := m.InspectSessions(supplier, miner.SessionState(q.Get("state")))
	if err != nil {
		s.fail(w, err)
		return
	}
	if id := q.Get("session"); id != "" {
		one := []map[string]string{}
		for _, sess := range sessions {
			if sess["session_id"] == id {
				one = append(one, sess)
			}
		}
		sessions = one
	}
	writeJSON(w, sessions)
}

func (s *Server) suppliers(w http.ResponseWriter, r *http.Request) {
	kb := s.src.KV.KB()
	keys, err := s.src.KV.ScanPrefix(r.Context(), kb.SupplierKeyPrefix()+":")
	if err != nil {
		s.fail(w, err)
		return
	}
	out := map[string]json.RawMessage{}
	for _, key := range keys {
		addr, ok := kb.SupplierStateAddress(key)
		if !ok {
			continue
		}
		value, err := s.src.KV.Get(r.Context(), key)
		if errors.Is(err, kv.ErrNotFound) {
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if json.Valid(value) {
			out[addr] = value
		}
	}
	writeJSON(w, out)
}

func (s *Server) streams(w http.ResponseWriter, r *http.Request) {
	if supplier := r.URL.Query().Get("supplier"); supplier != "" {
		st, err := s.src.Queues.Stats(supplier)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, []pebblequeue.QueueStats{st})
		return
	}
	all, err := s.src.Queues.AllStats()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, all)
}

func (s *Server) smst(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	session := q.Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	m := s.miner(w)
	if m == nil {
		return
	}
	trees, err := m.InspectSMST(q.Get("supplier"), session)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, trees)
}

func (s *Server) dedup(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	session := q.Get("session")
	if session == "" {
		http.Error(w, "session is required", http.StatusBadRequest)
		return
	}
	sample := defaultDedupSample
	if v := q.Get("sample"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "sample must be a count", http.StatusBadRequest)
			return
		}
		sample = n
	}
	m := s.miner(w)
	if m == nil {
		return
	}
	view, err := m.InspectDedup(session, sample)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, view)
}

// meter lists the session's meters, one per supplier; with no session, the
// key of every meter.
func (s *Server) meter(w http.ResponseWriter, r *http.Request) {
	kb := s.src.KV.KB()
	session := r.URL.Query().Get("session")
	if session == "" {
		keys, err := s.src.KV.ScanPrefix(r.Context(), kb.MeterSessionKey(""))
		if err != nil {
			s.fail(w, err)
			return
		}
		sort.Strings(keys)
		if keys == nil {
			keys = []string{}
		}
		writeJSON(w, keys)
		return
	}
	keys, err := s.src.KV.ScanPrefix(r.Context(), kb.MeterSessionKey(session)+":")
	if err != nil {
		s.fail(w, err)
		return
	}
	sort.Strings(keys)
	out := []MeterEntry{}
	for _, key := range keys {
		if !strings.HasSuffix(key, ":meta") {
			continue
		}
		meta, err := s.src.KV.Get(r.Context(), key)
		if errors.Is(err, kv.ErrNotFound) {
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if !json.Valid(meta) {
			meta, _ = json.Marshal(string(meta))
		}
		entry := MeterEntry{Key: key, Meta: meta}
		supplier := strings.TrimSuffix(strings.TrimPrefix(key, kb.MeterSessionKey(session)+":"), ":meta")
		consumed, err := s.src.KV.Get(r.Context(), kb.MeterConsumedKey(session, supplier))
		switch {
		case err == nil:
			c := string(consumed)
			entry.ConsumedUpokt = &c
		case !errors.Is(err, kv.ErrNotFound):
			s.fail(w, err)
			return
		}
		out = append(out, entry)
	}
	writeJSON(w, out)
}

// submissions returns the submission tracking records, the supplier's when
// one is given; the caller filters and sorts, as `redis submissions` does.
func (s *Server) submissions(w http.ResponseWriter, r *http.Request) {
	kb := s.src.KV.KB()
	prefix := strings.TrimSuffix(kb.TxTrackAllPattern(), "*")
	if supplier := r.URL.Query().Get("supplier"); supplier != "" {
		prefix = strings.TrimSuffix(kb.TxTrackPattern(supplier), "*")
	}
	keys, err := s.src.KV.ScanPrefix(r.Context(), prefix)
	if err != nil {
		s.fail(w, err)
		return
	}
	sort.Strings(keys)
	out := []json.RawMessage{}
	for _, key := range keys {
		value, err := s.src.KV.Get(r.Context(), key)
		if errors.Is(err, kv.ErrNotFound) {
			continue
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if json.Valid(value) {
			out = append(out, value)
		}
	}
	writeJSON(w, out)
}

// fail answers 500: an error reading the store is never an empty answer.
func (s *Server) fail(w http.ResponseWriter, err error) {
	s.logger.Debug().Err(err).Msg("inspect request failed")
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
