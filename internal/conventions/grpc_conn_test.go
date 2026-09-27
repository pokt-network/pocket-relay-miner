package conventions

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"testing"
)

// Every outbound gRPC connection to a full node is built by
// transport/grpcconn, so the query path and the transaction path cannot drift
// apart in what they configure.
//
// This check exists because they HAD drifted, invisibly: query/query.go set
// keepalive, windows, a receive limit, backoff and the stream observer, while
// tx/tx_client.go set credentials and nothing else. Nobody noticed for as long
// as the second branch never ran -- the miner handed the tx client the query
// connection -- and giving the tx client its own connection is exactly what
// makes an unconfigured dial reach the path that carries the money.
const grpcConnPackage = "transport/grpcconn/grpcconn.go"

// grpcNewClientExemptions are files allowed to dial on their own, each with a
// written reason. Adding one is a deliberate, reviewed edit.
var grpcNewClientExemptions = map[string]string{
	// These dial the RELAYER, not a full node: a local, operator-run process
	// reached over plaintext on purpose. Routing them through grpcconn would
	// hand them TLS credentials and a node-shaped keepalive, and the first
	// symptom would be a CLI that cannot reach its own relayer.
	"cmd/relay/grpc.go": "CLI probes target the local relayer over plaintext, not a full node",
}

// grpcNewClientCalls returns the positions of grpc.NewClient calls in a file.
func grpcNewClientCalls(f *ast.File, fset *token.FileSet) []string {
	var hits []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "NewClient" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "grpc" {
			return true
		}
		hits = append(hits, fset.Position(call.Pos()).String())
		return true
	})
	return hits
}

// TestGRPCDialsGoThroughOneConstructor fails on any grpc.NewClient outside
// transport/grpcconn that is not exempt, and on exemptions that no longer
// apply, so the list shrinks instead of rotting.
func TestGRPCDialsGoThroughOneConstructor(t *testing.T) {
	files, fset := goFiles(t, false)

	var violations []string
	seenExempt := map[string]bool{}
	constructorDials := 0

	for path, f := range files {
		hits := grpcNewClientCalls(f, fset)
		if len(hits) == 0 {
			continue
		}
		if path == grpcConnPackage {
			constructorDials += len(hits)
			continue
		}
		if _, exempt := grpcNewClientExemptions[path]; exempt {
			seenExempt[path] = true
			continue
		}
		for _, at := range hits {
			violations = append(violations, fmt.Sprintf("%s (%s)", at, path))
		}
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("grpc.NewClient outside %s (use grpcconn.New, or add an exemption with a reason):\n%s",
			grpcConnPackage, joinLines(violations))
	}
	// The constructor itself must dial, or this check is passing over a file
	// that no longer does what its name says.
	if constructorDials == 0 {
		t.Errorf("no grpc.NewClient found in %s: the single constructor is gone or moved", grpcConnPackage)
	}
	for path := range grpcNewClientExemptions {
		if !seenExempt[path] {
			t.Errorf("stale exemption %q: it no longer calls grpc.NewClient — remove it", path)
		}
	}
}
