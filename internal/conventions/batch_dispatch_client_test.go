package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The batching publisher writes through a Redis client of its own, so its writes
// never wait on the pool the cache and the meter share, and that client is closed
// only after the publisher's final flush has written through it.
func TestTheBatchingPublisherWritesThroughItsOwnClient(t *testing.T) {
	files, _ := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	var client ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && callsName(call, "NewBatchingPublisher") && len(call.Args) >= 2 {
			client = call.Args[1]
		}
		return true
	})
	if client == nil {
		t.Fatalf("%s: no NewBatchingPublisher call with a client argument", path)
	}
	sel, isSel := client.(*ast.SelectorExpr)
	if !isSel || !isIdentNamed(sel.X, "batchRedisClient") {
		t.Errorf("%s: NewBatchingPublisher must be given batchRedisClient, the dispatch's own client, "+
			"not the shared pool the cache and the meter wait on", path)
	}

	// The batch client is opened by openRedisRelayPublisher, which hands its
	// Close back as closeBatchClient; serveRelayer defers that before the
	// publisher's Close.
	if !closesInFuncLit(f, "closeClient", "batchRedisClient") {
		t.Fatalf("%s: closeClient, the close openRedisRelayPublisher returns, must close batchRedisClient", path)
	}
	closed := deferredCallOf(f, "closeBatchClient")
	publisher := deferredCloseOf(f, "publisher")
	if closed == token.NoPos {
		t.Fatalf("%s has no deferred closeBatchClient()", path)
	}
	if publisher <= closed {
		t.Errorf("%s defers publisher.Close() before closeBatchClient(): defers run LIFO, so the "+
			"batch client would be closed when the final flush writes through it", path)
	}
}

// deferredCallOf is the position of the first `defer name()`.
func deferredCallOf(f *ast.File, name string) token.Pos {
	pos := token.NoPos
	ast.Inspect(f, func(n ast.Node) bool {
		if d, ok := n.(*ast.DeferStmt); ok && pos == token.NoPos && isIdentNamed(d.Call.Fun, name) {
			pos = d.Pos()
		}
		return pos == token.NoPos
	})
	return pos
}

// closesInFuncLit reports whether `name := func() { ... v.Close() ... }` is in f.
func closesInFuncLit(f *ast.File, name, v string) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 || !isIdentNamed(as.Lhs[0], name) {
			return !found
		}
		lit, ok := as.Rhs[0].(*ast.FuncLit)
		if !ok {
			return !found
		}
		ast.Inspect(lit.Body, func(inner ast.Node) bool {
			if call, ok := inner.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" && isIdentNamed(sel.X, v) {
					found = true
				}
			}
			return !found
		})
		return !found
	})
	return found
}
