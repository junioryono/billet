package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// EVERY OPEN OF THE LEDGER NAMES THE RUNNING RELEASE.
//
// The release watermark refuses a proved downgrade and lets the control plane
// record a proved upgrade, and both depend on the opener saying which billet it
// is — an open that names nothing gets neither. That is right for a test opening
// a throwaway ledger and wrong for every command in this program, and nothing
// at run time would notice the omission: the ledger opens, the schema verifies,
// and an older binary quietly serves rows a newer one wrote.
//
// A STRUCTURAL TEST BECAUSE ledger.go IS THE ONE PLACE THAT OPENS. Every command
// reaches the store through it, so proving each state.Open* call there carries
// state.WithRunningRelease is proving it for the program — and the day somebody
// adds a seventh open helper without the option, this names the line.
func TestEveryLedgerOpenNamesTheRunningRelease(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "ledger.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ledger.go: %v", err)
	}

	var opens int

	// THE ONE EXEMPTION, BY NAME: the instruction reader. A standby's timer is an
	// older binary reading what it should become, and the watermark the newer
	// leader recorded would refuse it. Its opens are asserted the other way
	// round below: they must NOT name a release.
	var exempt *ast.FuncDecl

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "openStateForDecision" {
			exempt = fn
		}
	}

	if exempt == nil {
		t.Fatal("ledger.go has no openStateForDecision; the timer's instruction read moved")
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		if call.Pos() >= exempt.Pos() && call.End() <= exempt.End() {
			if pkg, name, ok := selector(call.Fun); ok && pkg == "state" && len(name) >= 4 &&
				name[:4] == "Open" {
				// AND THE OPERATOR OPENERS, on both backends: a probe or a
				// maintenance open there would read past the fence or refuse to
				// migrate an unheld ledger, neither of which is what an operator's
				// read does.
				if name != "OpenAdmin" && name != "OpenPostgresAdmin" {
					t.Errorf("%s: openStateForDecision opens through state.%s, want the operator "+
						"open of its backend", fset.Position(call.Pos()), name)
				}

				for _, arg := range call.Args {
					if inner, ok := arg.(*ast.CallExpr); ok {
						if p, fn, ok := selector(inner.Fun); ok && p == "state" &&
							fn == "WithRunningRelease" {
							t.Errorf("%s: openStateForDecision names the running release, so a "+
								"standby behind its leader cannot read its own instruction",
								fset.Position(call.Pos()))
						}
					}
				}
			}

			return true
		}

		pkg, name, ok := selector(call.Fun)
		if !ok || pkg != "state" || len(name) < 4 || name[:4] != "Open" {
			return true
		}

		opens++

		for _, arg := range call.Args {
			inner, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}

			if p, fn, ok := selector(inner.Fun); ok && p == "state" && fn == "WithRunningRelease" {
				return true
			}
		}

		t.Errorf("%s: state.%s is called without state.WithRunningRelease, so this open "+
			"neither refuses a downgrade nor records an upgrade", fset.Position(call.Pos()), name)

		return true
	})

	// THE COUNT IS ASSERTED, or a refactor that moved every open out of this
	// file would leave a test that inspects nothing and passes.
	if opens < 6 {
		t.Fatalf("found %d state.Open* calls in ledger.go, want the six open helpers; if they "+
			"moved, move this test with them", opens)
	}
}

// selector reads `pkg.Name` out of a call's function expression.
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
