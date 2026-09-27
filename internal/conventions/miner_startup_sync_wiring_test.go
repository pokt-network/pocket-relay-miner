package conventions

import (
	"go/ast"
	"go/token"
	"testing"
)

// The miner reads the node's network, the shared params and the committed
// height before it consumes a single relay, and does not start without them.
// SupplierWorker.Start builds the whole worker against a live node and has no
// test, so the order is frozen by reading it.

// minerStartupSyncViolations reports what is wrong with the startup chain read in
// (*SupplierWorker).Start in f.
func minerStartupSyncViolations(f *ast.File) []string {
	var start *ast.FuncDecl
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "Start" || fd.Recv == nil || len(fd.Recv.List) != 1 || fd.Body == nil {
			continue
		}
		if star, ok := fd.Recv.List[0].Type.(*ast.StarExpr); ok && isIdentNamed(star.X, "SupplierWorker") {
			start = fd
		}
	}
	if start == nil {
		return []string{"no (*SupplierWorker).Start found"}
	}

	var (
		reads          []*ast.CallExpr
		heightVar      string
		returnsOnError bool
		seed           = token.NoPos
		managerStart   = token.NoPos
	)
	// The body block is visited before anything inside it, so the height
	// variable is known before any seeding call is looked at.
	ast.Inspect(start.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			for i, stmt := range n.List {
				assign, ok := stmt.(*ast.AssignStmt)
				// params, height, block time, error: the block time joined them
				// when the anchor of every transaction started being read here.
				if !ok || len(assign.Lhs) != 4 || len(assign.Rhs) != 1 {
					continue
				}
				call, ok := assign.Rhs[0].(*ast.CallExpr)
				if !ok || !callsName(call, "readStartupChainState") {
					continue
				}
				if id, ok := assign.Lhs[1].(*ast.Ident); ok {
					heightVar = id.Name
				}
				if id, ok := assign.Lhs[3].(*ast.Ident); ok && i+1 < len(n.List) {
					returnsOnError = ifErrorReturns(n.List[i+1], id.Name)
				}
			}
		case *ast.CallExpr:
			if callsName(n, "readStartupChainState") {
				reads = append(reads, n)
			}
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "seedChainState" && len(n.Args) == 2 &&
				heightVar != "" && isIdentNamed(n.Args[0], heightVar):
				seed = n.Pos()
			case sel.Sel.Name == "Start" && selectsField(sel.X, "supplierManager"):
				managerStart = n.Pos()
			}
		}
		return true
	})

	if len(reads) != 1 {
		return []string{"readStartupChainState must be called exactly once in Start"}
	}
	read := reads[0]
	var out []string
	if len(read.Args) < 2 || !selectsField(read.Args[1], "ChainID") {
		out = append(out, "readStartupChainState must be given the configured ChainID, the one transactions are signed for")
	}
	if !returnsOnError {
		out = append(out, "Start must return right after readStartupChainState fails")
	}
	if seed == token.NoPos || seed < read.Pos() {
		out = append(out, "the chain state read at startup must be seeded (seedChainState) with the height readStartupChainState returned, after reading it and before consuming")
	}
	if managerStart == token.NoPos || managerStart < seed {
		out = append(out, "the supplier manager, which starts consuming, must start after the block adapter is seeded")
	}
	return out
}

// ifErrorReturns reports whether stmt is `if errVar != nil { ... return ... }`.
func ifErrorReturns(stmt ast.Stmt, errVar string) bool {
	ifStmt, ok := stmt.(*ast.IfStmt)
	if !ok {
		return false
	}
	cond, ok := ifStmt.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ || !isIdentNamed(cond.X, errVar) || !isIdentNamed(cond.Y, "nil") {
		return false
	}
	for _, s := range ifStmt.Body.List {
		if _, ok := s.(*ast.ReturnStmt); ok {
			return true
		}
	}
	return false
}

func TestTheMinerDoesNotConsumeBeforeItHasReadTheChain(t *testing.T) {
	files, _ := goFiles(t, false)

	const path = "miner/supplier_worker.go"
	f, ok := files[path]
	if !ok {
		t.Fatalf("%s not found: if it moved, point this rule at its new path", path)
	}
	for _, v := range minerStartupSyncViolations(f) {
		t.Errorf("%s: %s", path, v)
	}
}

// TestMinerStartupSyncViolationsReadsTheShape proves the rule reads the error
// return, the chain ID, the seeded height and the order, not merely the names.
func TestMinerStartupSyncViolationsReadsTheShape(t *testing.T) {
	const read = `
	sharedParams, startHeight, startBlockTime, err := readStartupChainState(w.ctx, w.config.ChainID, readers)
	if err != nil {
		w.cleanup()
		return err
	}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"wired", read + `
	w.seedChainState(startHeight, startBlockTime)
	w.supplierManager.Start(ctx)`, 0},
		{"error only logged", `
	sharedParams, startHeight, startBlockTime, err := readStartupChainState(w.ctx, w.config.ChainID, readers)
	if err != nil {
		w.logger.Warn().Err(err).Msg("chain unreadable")
	}
	w.seedChainState(startHeight, startBlockTime)
	w.supplierManager.Start(ctx)`, 1},
		{"not given the configured chain id", `
	sharedParams, startHeight, startBlockTime, err := readStartupChainState(w.ctx, "pocket", readers)
	if err != nil {
		return err
	}
	w.seedChainState(startHeight, startBlockTime)
	w.supplierManager.Start(ctx)`, 1},
		{"seeded with another height", read + `
	w.seedChainState(0, startBlockTime)
	w.supplierManager.Start(ctx)`, 1},
		{"consumes before seeding", read + `
	w.supplierManager.Start(ctx)
	w.seedChainState(startHeight, startBlockTime)`, 1},
		{"never read", `
	w.supplierManager.Start(ctx)`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := parseSource(t, "package miner\n\nfunc (w *SupplierWorker) Start(ctx context.Context) error {"+tc.body+"\n\treturn nil\n}\n")
			if got := minerStartupSyncViolations(f); len(got) != tc.want {
				t.Fatalf("want %d violations, got %d: %v", tc.want, len(got), got)
			}
		})
	}
}
