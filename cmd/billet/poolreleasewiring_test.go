package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A POOL'S RUN ASKS poolReleaseOwed, AND NOTHING ELSE, BEFORE IT WAITS FOR THE POOL.
//
// poolReleaseOwed's own test cannot see whether runAcceptance uses it: a guard put back
// to `waitErr == nil && ...` would leave it green, and a pool's run that gave up on a
// job GitHub skipped would again seal hosts the pool had not yet let go (run
// 43214914994). The call cannot be reached from a unit test without a GitHub and a
// pool, so the source is asserted: in runAcceptance, exactly one call to
// waitForPoolReleased, inside an if whose whole condition is poolReleaseOwed(ctx, opts).
func TestAPoolsRunGuardsItsReleaseWaitWithPoolReleaseOwedAlone(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "acceptancerun.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "runAcceptance" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("acceptancerun.go has no runAcceptance")
	}

	waits, guarded := 0, 0
	ast.Inspect(run.Body, func(n ast.Node) bool {
		if stmt, ok := n.(*ast.IfStmt); ok && isPoolReleaseOwedCall(stmt.Cond) {
			ast.Inspect(stmt.Body, func(n ast.Node) bool {
				if isCallTo(n, "waitForPoolReleased") {
					guarded++
				}

				return true
			})
		}
		if isCallTo(n, "waitForPoolReleased") {
			waits++
		}

		return true
	})

	if waits != 1 || guarded != 1 {
		t.Fatalf("runAcceptance waits for the pool's release %d time(s), %d of them under "+
			"`if poolReleaseOwed(ctx, opts)` alone; want exactly one, guarded so", waits, guarded)
	}
}

// isPoolReleaseOwedCall says whether cond is exactly poolReleaseOwed(ctx, opts).
func isPoolReleaseOwedCall(cond ast.Expr) bool {
	call, ok := cond.(*ast.CallExpr)
	if !ok || !isCallTo(call, "poolReleaseOwed") || len(call.Args) != 2 {
		return false
	}
	for i, want := range []string{"ctx", "opts"} {
		if id, ok := call.Args[i].(*ast.Ident); !ok || id.Name != want {
			return false
		}
	}

	return true
}

// isCallTo says whether n calls the package-level function name.
func isCallTo(n ast.Node, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)

	return ok && id.Name == name
}
