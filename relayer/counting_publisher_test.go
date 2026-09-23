//go:build test

package relayer

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	servicetypes "github.com/pokt-network/poktroll/x/service/types"

	"github.com/pokt-network/pocket-relay-miner/transport"
)

// failingPublisher refuses every relay.
type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, *transport.MinedRelayMessage) error {
	return errors.New("injected: the store refused it")
}
func (failingPublisher) Close() error { return nil }

func TestCountPublished_WrapsOnceAndLeavesNilAlone(t *testing.T) {
	require.Nil(t, countPublished(nil), "a nil publisher must stay nil: every transport reads it as publish nothing")
	once := countPublished(&recordingPublisher{})
	_, ok := once.(*countingPublisher)
	require.True(t, ok)
	require.Same(t, once, countPublished(once), "wrapping a wrapped publisher again would count every relay twice")
}

func TestCountingPublisher_CountsOnlyWhatTheStoreAccepted(t *testing.T) {
	msg := &transport.MinedRelayMessage{ServiceId: "svc-count", SupplierOperatorAddress: "pokt1count"}
	published := relaysPublished.WithLabelValues("svc-count", "pokt1count", metricLabelUnknown)
	before := testutil.ToFloat64(published)

	require.Error(t, countPublished(failingPublisher{}).Publish(context.Background(), msg))
	require.Equal(t, before, testutil.ToFloat64(published), "a refused publish is not a published relay")

	require.NoError(t, countPublished(&recordingPublisher{}).Publish(context.Background(), msg))
	require.Equal(t, before+1, testutil.ToFloat64(published))

	typed := relaysPublished.WithLabelValues("svc-count", "pokt1count", BackendTypeJSONRPC)
	typedBefore := testutil.ToFloat64(typed)
	require.NoError(t, countPublished(&recordingPublisher{}).Publish(WithRPCType(context.Background(), BackendTypeJSONRPC), msg))
	require.Equal(t, typedBefore+1, testutil.ToFloat64(typed), "the transport on the context labels the count")
	require.Equal(t, before+1, testutil.ToFloat64(published), "a labelled publish must not also count as unknown")
}

// TestRelaysPublished_AWebSocketRelayCountsOnce: the bridge gets the proxy's
// publisher, already wrapped, and wraps it again.
func TestRelaysPublished_AWebSocketRelayCountsOnce(t *testing.T) {
	verifyNoBridgeGoroutines(t)
	backendURL, _, _ := countingWSBackend(t)
	pipeline, _, _ := newOwnerTestPipeline(t)
	supplier, signer := newSupplier(t)
	relayerConn, _ := newGatewaySideHarness(t)

	bridge, err := NewWebSocketBridge(
		testLogger(), relayerConn, backendURL, simWSTestService, "", atHeight(100),
		&recordingProcessor{}, countPublished(&ctxWatchingPublisher{}), signer, http.Header{},
		nil, pipeline, 5*time.Second, false, nil, "", nil, nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bridge.Close() })
	bridge.owner.Store(&supplier)
	published := relaysPublished.WithLabelValues(simWSTestService, supplier, BackendTypeWebSocket)
	before := testutil.ToFloat64(published)

	bridge.emitRelay(ownerTestRelay("ws-published", supplier), &servicetypes.RelayResponse{}, []byte(`{"ok":true}`))

	require.Equal(t, before+1, testutil.ToFloat64(published),
		"a WebSocket relay reaching the store counts in relays_published_total, once")
}

// newOKBackend answers every request with a JSON-RPC result.
func newOKBackend(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","result":"0x10","id":1}`))
	}))
	t.Cleanup(s.Close)
	return s
}

// TestRelaysPublished_AGRPCRelayCountsOnce goes through the real handler.
func TestRelaysPublished_AGRPCRelayCountsOnce(t *testing.T) {
	fx := newGRPCPublishFixture(t, newOKBackend(t).URL)
	published := relaysPublished.WithLabelValues("develop-http", "pokt1testsupplieroperator", BackendTypeGRPC)
	before := testutil.ToFloat64(published)

	require.NoError(t, fx.svc.handleSendRelay(fx.stream))

	require.Equal(t, int32(1), fx.pub.calls.Load(), "premise: the relay was published")
	require.Equal(t, before+1, testutil.ToFloat64(published),
		"a gRPC relay reaching the store counts in relays_published_total, once")
}

// TestRelaysPublished_AGRPCRelayWithNoPublisherIsADrop: mined and never
// handed to the store.
func TestRelaysPublished_AGRPCRelayWithNoPublisherIsADrop(t *testing.T) {
	fx := newGRPCPublishFixture(t, newOKBackend(t).URL)
	fx.svc.publisher = nil
	dropped := relaysDropped.WithLabelValues("develop-http", BackendTypeGRPC, dropReasonNoPublisher)
	before := testutil.ToFloat64(dropped)

	require.NoError(t, fx.svc.handleSendRelay(fx.stream))

	require.Equal(t, before+1, testutil.ToFloat64(dropped), "a mined relay with no publisher is counted as dropped")
}

// TestRelaysPublished_AnHTTPRelayCountsOnce: the HTTP path used to count after
// its own Publish; the decorator counts now, and only the decorator.
func TestRelaysPublished_AnHTTPRelayCountsOnce(t *testing.T) {
	p := &ProxyServer{logger: testLogger(), relayProcessor: &recordingProcessor{}, publisher: countPublished(&recordingPublisher{})}
	published := relaysPublished.WithLabelValues("svc-http-once", "pokt1httponce", BackendTypeJSONRPC)
	before := testutil.ToFloat64(published)

	p.executePublish(context.Background(), publishTask{serviceID: "svc-http-once", supplierAddr: "pokt1httponce", rpcType: BackendTypeJSONRPC})

	require.Equal(t, before+1, testutil.ToFloat64(published),
		"an HTTP relay reaching the store counts in relays_published_total, once")
}

// TestRelaysPublished_AnHTTPRelayWithNoPublisherIsADrop: the HTTP path gives
// up on a relay with no publisher before mining it, and counts it the same way.
func TestRelaysPublished_AnHTTPRelayWithNoPublisherIsADrop(t *testing.T) {
	p := &ProxyServer{logger: testLogger()}
	dropped := relaysDropped.WithLabelValues("svc-http-nopub", BackendTypeJSONRPC, dropReasonNoPublisher)
	before := testutil.ToFloat64(dropped)

	p.executePublish(context.Background(), publishTask{serviceID: "svc-http-nopub", supplierAddr: "pokt1nopub", rpcType: BackendTypeJSONRPC})

	require.Equal(t, before+1, testutil.ToFloat64(dropped), "a relay with no publisher is counted as dropped")
}

// TestPublish_EveryPublishOfAMinedRelayIsCounted type-checks the relayer's
// non-test files: every Publish whose receiver is a transport.MinedRelayPublisher
// must be countingPublisher's own inner call, or be made on a struct field that
// is only ever assigned countPublished(...). A new transport, or a new call
// site, that publishes around the decorator fails here, not in a load report.
func TestPublish_EveryPublishOfAMinedRelayIsCounted(t *testing.T) {
	goList := func(args ...string) string {
		out, err := exec.Command("go", append([]string{"list"}, args...)...).Output()
		require.NoError(t, err, "go list %v", args)
		return strings.TrimSpace(string(out))
	}
	exports := map[string]string{}
	for _, line := range strings.Split(goList("-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", "."), "\n") {
		path, file, _ := strings.Cut(line, "\t")
		exports[path] = file
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range strings.Fields(goList("-f", `{{join .GoFiles " "}}`, ".")) {
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if f := exports[path]; f != "" {
			return os.Open(f)
		}
		return nil, fmt.Errorf("no export data for %s", path)
	})}
	pkg, err := conf.Check(goList("-f", "{{.ImportPath}}", "."), fset, files, info)
	require.NoError(t, err)

	var publisherType types.Type
	for _, imp := range pkg.Imports() {
		if strings.HasSuffix(imp.Path(), "/transport") {
			if obj := imp.Scope().Lookup("MinedRelayPublisher"); obj != nil {
				publisherType = obj.Type()
			}
		}
	}
	require.NotNil(t, publisherType, "control: transport.MinedRelayPublisher must be found")

	fields := map[types.Object]bool{}
	examined := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			inDecorator := fn.Name.Name == "Publish" && fn.Recv != nil &&
				strings.Contains(types.ExprString(fn.Recv.List[0].Type), "countingPublisher")
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Publish" {
					return true
				}
				s := info.Selections[sel]
				if s == nil || !types.Identical(s.Recv(), publisherType) {
					return true
				}
				examined++
				if inDecorator {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); ok {
					if fs := info.Selections[inner]; fs != nil && fs.Kind() == types.FieldVal {
						fields[fs.Obj()] = true
						return true
					}
				}
				t.Errorf("%s: Publish on a publisher that is not a wrapped field", fset.Position(call.Pos()))
				return true
			})
		}
	}
	require.GreaterOrEqual(t, examined, 1, "control: the walk must examine at least one Publish")

	// Every write into those fields must be countPublished(...).
	isWrap := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		id, ok := c.Fun.(*ast.Ident)
		return ok && id.Name == "countPublished"
	}
	writes := map[types.Object]int{}
	for _, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.KeyValueExpr:
				if id, ok := n.Key.(*ast.Ident); ok && fields[info.Uses[id]] {
					writes[info.Uses[id]]++
					if !isWrap(n.Value) {
						t.Errorf("%s: %s is set without countPublished", fset.Position(n.Pos()), id.Name)
					}
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if s, ok := lhs.(*ast.SelectorExpr); ok {
						if fs := info.Selections[s]; fs != nil && fields[fs.Obj()] {
							writes[fs.Obj()]++
							if i < len(n.Rhs) && !isWrap(n.Rhs[i]) {
								t.Errorf("%s: %s is assigned without countPublished", fset.Position(n.Pos()), s.Sel.Name)
							}
						}
					}
				}
			}
			return true
		})
	}
	for f := range fields {
		require.Positivef(t, writes[f], "control: the walk must find where %s is set", f.Name())
	}
}
