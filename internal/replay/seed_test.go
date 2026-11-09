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

	var seeded, opened []token.Pos

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		pkg, ok := fn.X.(*ast.Ident)
		if !ok {
			return true
		}

		switch {
		case pkg.Name == "ledgertest" && fn.Sel.Name == "Seed":
			seeded = append(seeded, call.Pos())
		case pkg.Name == "app" && fn.Sel.Name == "OpenControlPlane":
			opened = append(opened, call.Pos())
		}

		return true
	})

	if len(seeded) != 1 || len(opened) != 1 {
		t.Fatalf("stack.go seeds %d ledger(s) and opens %d control plane(s); want one of each",
			len(seeded), len(opened))
	}

	if seeded[0] >= opened[0] {
		t.Fatalf("the ledger is seeded at %s, after the control plane opens at %s",
			fset.Position(seeded[0]), fset.Position(opened[0]))
	}
}
