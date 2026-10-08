package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// EACH OF cmd/billet's LEDGER OPENERS IS THE MODE ITS NAME SAYS. The opens are
// internal/app's, held there to the release rules; these names are what the
// commands choose between, so one naming the wrong mode would open, say, the
// inspection handle where a command writes, and nothing would say so until it
// failed to.
func TestEachLedgerOpenerIsTheModeItsNameSays(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "ledger.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ledger.go: %v", err)
	}

	want := map[string][2]string{
		"openStateForDecision": {"OpenLedger", "LedgerDecision"},
		"openStateAdmin":       {"OpenLedger", "LedgerOperator"},
		"openStateInspect":     {"OpenLedgerWith", "LedgerInspect"},
		"openStateMaintenance": {"OpenLedger", "LedgerMaintenance"},
	}

	found := map[string]bool{}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}

		w, ok := want[fn.Name.Name]
		if !ok {
			continue
		}

		found[fn.Name.Name] = true

		if len(fn.Body.List) != 1 {
			t.Errorf("%s is %d statements, want the one return of its mode's open", fn.Name.Name, len(fn.Body.List))

			continue
		}

		ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			t.Errorf("%s does not return its mode's open", fn.Name.Name)

			continue
		}

		call, ok := ret.Results[0].(*ast.CallExpr)
		if !ok {
			t.Errorf("%s does not return a call", fn.Name.Name)

			continue
		}

		pkg, name, _ := selector(call.Fun)
		if pkg != "app" || name != w[0] || len(call.Args) < 3 {
			t.Errorf("%s returns %s.%s, want app.%s", fn.Name.Name, pkg, name, w[0])

			continue
		}

		if p, mode, ok := selector(call.Args[2]); !ok || p != "app" || mode != w[1] {
			t.Errorf("%s opens in mode %v, want app.%s", fn.Name.Name, call.Args[2], w[1])
		}
	}

	for name := range want {
		if !found[name] {
			t.Errorf("ledger.go has no %s", name)
		}
	}
}

// selector reads `pkg.Name` out of an expression.
func selector(expr ast.Expr) (string, string, bool) {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}

	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", "", false
	}

	return pkg.Name, sel.Sel.Name, true
}
