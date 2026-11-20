package nodeclient_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE CONTROL PLANE'S TEND AND SWEEP RUN UNDER THE CUSTODY MUTEX IN THE DESTROYS' LINE,
// and release it before their report. A launch taken since may be taking custody of a
// report it lost, and a sweep that ran beside that would see its retirement of the lease
// overwritten. The behavioural test can stage the interleaving but not prove the lock
// blocked, since a goroutine may pause between its hook and the lock; this holds the
// order in the source.
func TestTheQueuedTendAndSweepHoldCustodyAcrossTheirExecution(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "loop.go", nil, 0)
	if err != nil {
		t.Fatalf("parse loop.go: %v", err)
	}

	var body *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isCall(call, "destroys", "after") || len(call.Args) != 2 {
			return true
		}
		if lit, ok := call.Args[1].(*ast.FuncLit); ok {
			body = lit.Body
		}

		return true
	})
	if body == nil {
		t.Fatal("found no destroys.after(..., func() error {...}) in loop.go")
	}

	var lock, execute, unlock, report token.Pos
	for _, stmt := range body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			switch {
			case isCall(call, "custody", "Lock") && lock == token.NoPos:
				lock = call.Pos()
			case isIdentCall(call, "executePolled"):
				execute = call.Pos()
			case isCall(call, "custody", "Unlock") && execute != token.NoPos && unlock == token.NoPos:
				unlock = call.Pos()
			case isIdentCall(call, "reportOutcome"):
				report = call.Pos()
			}

			return true
		})
	}

	switch {
	case lock == token.NoPos || execute == token.NoPos || unlock == token.NoPos || report == token.NoPos:
		t.Fatalf("the queued branch is missing a step: lock %v, executePolled %v, unlock after it %v, "+
			"reportOutcome %v", lock.IsValid(), execute.IsValid(), unlock.IsValid(), report.IsValid())
	case lock >= execute || execute >= unlock || unlock >= report:
		t.Fatalf("the queued branch runs out of order: lock at %s, executePolled at %s, unlock at %s, "+
			"reportOutcome at %s; want them in that order", fset.Position(lock), fset.Position(execute),
			fset.Position(unlock), fset.Position(report))
	}
}

func isCall(call *ast.CallExpr, receiver, method string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == receiver
}

func isIdentCall(call *ast.CallExpr, name string) bool {
	id, ok := call.Fun.(*ast.Ident)

	return ok && id.Name == name
}
