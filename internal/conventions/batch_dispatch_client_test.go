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

	closed := deferredCloseOf(f, "batchRedisClient")
	publisher := deferredCloseOf(f, "publisher")
	if closed == token.NoPos {
		t.Fatalf("%s has no deferred batchRedisClient.Close()", path)
	}
	if publisher <= closed {
		t.Errorf("%s defers publisher.Close() before batchRedisClient.Close(): defers run LIFO, so the "+
			"batch client would be closed when the final flush writes through it", path)
	}
}
