package replay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE HARNESS SEEDS ITS LEDGER BEFORE IT OPENS THE CONTROL PLANE. Without the
// seed every replay migrates a ledger from empty under the production startup
// budget, which a loaded race-and-coverage run missed for every replay in it
// (#188), and nothing else would notice the seed was gone: the replays still
// pass when the machine is idle.
func TestTheHarnessSeedsItsLedgerBeforeItOpens(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "stack.go", nil, 0)
	if err != nil {
		t.Fatalf("parse stack.go: %v", err)
	}

	// IN ONE FUNCTION, OF THE DIRECTORY THE OPEN READS: the seed's directory is
	// cfg.Server.IdentityDir, where the open finds billet.db, and the open is of
	// that same cfg, as a statement of the function's own body, not a branch.
	found := false

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}

		seeded, opened := token.NoPos, token.NoPos

		for _, stmt := range fn.Body.List {
			ast.Inspect(stmt, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				switch {
				case isCall(call, "ledgertest", "Seed") && len(call.Args) == 2 &&
					render(call.Args[1]) == "cfg.Server.IdentityDir":
					if _, unconditional := stmt.(*ast.ExprStmt); unconditional && seeded == token.NoPos {
						seeded = call.Pos()
					}
				case isCall(call, "app", "OpenControlPlane") && len(call.Args) > 1 && render(call.Args[1]) == "cfg":
					if opened == token.NoPos {
						opened = call.Pos()
					}
				}

				return true
			})
		}

		if opened == token.NoPos {
			continue
		}

		found = true

		if seeded == token.NoPos || seeded >= opened {
			t.Fatalf("%s opens the control plane at %s without first seeding cfg.Server.IdentityDir "+
				"unconditionally in its own body", fn.Name.Name, fset.Position(opened))
		}
	}

	if !found {
		t.Fatal("stack.go opens no control plane with cfg")
	}
}

// isCall reports whether call is pkg.name(...).
func isCall(call *ast.CallExpr, pkg, name string) bool {
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	ident, ok := fn.X.(*ast.Ident)

	return ok && ident.Name == pkg && fn.Sel.Name == name
}

// render spells an identifier or a selector chain as source does.
func render(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return render(e.X) + "." + e.Sel.Name
	default:
		return ""
	}
}
