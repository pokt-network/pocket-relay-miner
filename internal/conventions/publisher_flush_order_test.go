package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The relayer's mined-relay publisher may be a BatchingPublisher, which holds
// relays in memory between writes. Its Close() performs the FINAL FLUSH, and
// that flush writes THROUGH the Redis client -- p.client.TxPipelined in
// transport/redis/batching_publisher.go.
//
// So the two deferred Close() calls in runHARelayer are ordered, and the order
// is the opposite of the one they are written in: defers run LIFO, so the
// publisher's Close must be DECLARED AFTER the Redis client's in order to RUN
// BEFORE it.
//
// Get it backwards and nothing complains. The client's pool returns
// pool.ErrClosed ("redis: client is closed", internal/pool/pool.go:57-58,
// returned from getConn at :623) for every command after Close, so the flush's
// TxPipelined fails, dispatchAll puts the chunk back on a queue nobody will
// drain again, and the process exits. Every relay in it was served, signed and
// answered to a client, and it is never written: money served and not billed.
// No test covers it -- runHARelayer builds a whole process and has none -- and
// no run shows it either, because the loss only happens on a shutdown that had
// a backlog.
//
// This is the same shape as TestMissingCauseDeciderStaysWired: a startup
// decision provable only by reading, frozen here so that changing it goes red.

// deferredCloseOf returns the position of the `defer` statement that closes the
// variable named v, whether written as `defer v.Close()` or wrapped in a
// closure. token.NoPos means there is none.
func deferredCloseOf(f *ast.File, v string) token.Pos {
	pos := token.NoPos
	ast.Inspect(f, func(n ast.Node) bool {
		if pos != token.NoPos {
			return false
		}
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		// Look INSIDE the deferred expression: the repository writes these as
		// `defer func() { _ = x.Close() }()` to swallow the error.
		ast.Inspect(d, func(inner ast.Node) bool {
			if pos != token.NoPos {
				return false
			}
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "Close" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == v {
				pos = d.Pos()
				return false
			}
			return true
		})
		return pos == token.NoPos
	})
	return pos
}

func TestPublisherFlushIsDeferredAfterTheRedisClient(t *testing.T) {
	files, _ := goFiles(t, false)

	const path = "cmd/cmd_relayer.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}

	client := deferredCloseOf(f, "redisClient")
	publisher := deferredCloseOf(f, "publisher")

	if client == token.NoPos {
		t.Fatalf("%s has no deferred redisClient.Close().\n"+
			"  If the client is now closed some other way, this rule has to be rewritten\n"+
			"  against that shape -- not deleted: the ordering it protects still exists.", path)
	}
	if publisher == token.NoPos {
		t.Fatalf("%s has no deferred publisher.Close().\n"+
			"  The batching publisher flushes what it still holds in Close(). Without this\n"+
			"  defer, a shutdown with a backlog drops every relay in it.", path)
	}
	if publisher <= client {
		t.Errorf("%s defers publisher.Close() BEFORE redisClient.Close().\n"+
			"  Defers run LIFO, so declaring the publisher's first makes it run LAST: the\n"+
			"  Redis client is already closed when the final flush tries to write through\n"+
			"  it, every command returns redis: client is closed, and the queued chunk is\n"+
			"  put back on a queue nobody drains again. Those relays were served, signed\n"+
			"  and answered -- they are simply never written.\n"+
			"  Declare the publisher's defer AFTER the client's.", path)
	}
}

// TestDeferredCloseMatcherCatchesHostileShapes proves the matcher reads a
// DEFERRED Close of that variable, and not a Close it merely sees nearby.
func TestDeferredCloseMatcherCatchesHostileShapes(t *testing.T) {
	wrapped := `package x

func a() { defer func() { _ = publisher.Close() }() }
`
	bare := `package x

func a() { defer publisher.Close() }
`
	notDeferred := `package x

func a() { _ = publisher.Close() }
`
	otherVar := `package x

func a() { defer func() { _ = redisClient.Close() }() }
`
	mentioned := `package x

// publisher.Close() is only named here
var s = "defer publisher.Close()"
`
	for _, src := range []string{wrapped, bare} {
		f, _ := parseSource(t, src)
		if deferredCloseOf(f, "publisher") == token.NoPos {
			t.Fatalf("matcher missed a real deferred close in:\n%s", src)
		}
	}
	for _, src := range []string{notDeferred, otherVar, mentioned} {
		f, _ := parseSource(t, src)
		if deferredCloseOf(f, "publisher") != token.NoPos {
			t.Fatalf("matcher fired on something that is not a deferred publisher.Close() in:\n%s", src)
		}
	}

	// And the ordering itself: two defers in one function, read in source order.
	ordered := `package x

func a() {
	defer func() { _ = redisClient.Close() }()
	defer func() { _ = publisher.Close() }()
}
`
	f, _ := parseSource(t, ordered)
	if c, p := deferredCloseOf(f, "redisClient"), deferredCloseOf(f, "publisher"); p <= c {
		t.Fatal("matcher does not order two defers by source position")
	}
}
