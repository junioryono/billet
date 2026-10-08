package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// EVERY OPEN OF THE LEDGER NAMES THE RUNNING RELEASE, BUT THE DECISION READ.
//
// The release watermark refuses a proved downgrade and lets the control plane
// record a proved upgrade, and both depend on the opener saying which billet it
// is; an open that names nothing gets neither, and nothing at run time notices.
// ledger.go is where billet opens the ledger, every mode, so every state.Open*
// call there must carry state.WithRunningRelease(version.Version()), and each
// open the modes make is required by name, or a count would be satisfied by
// any nine.
//
// THE ONE EXEMPTION, BY NAME: the decision read (openDecisionLedger). A
// standby's timer is an older binary reading what it should become, and the
// watermark the newer leader recorded would refuse it. Its opens are asserted
// the other way round: the operator open of each backend, naming no release.
func TestEveryLedgerOpenNamesTheRunningRelease(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "ledger.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ledger.go: %v", err)
	}

	var exempt *ast.FuncDecl

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "openDecisionLedger" {
			exempt = fn
		}
	}

	if exempt == nil {
		t.Fatal("ledger.go has no openDecisionLedger; the timer's instruction read moved")
	}

	seen, decision := map[string]int{}, map[string]int{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		pkg, name, ok := selectorOf(call.Fun)
		if !ok || pkg != "state" || len(name) < 4 || name[:4] != "Open" {
			return true
		}

		if call.Pos() >= exempt.Pos() && call.End() <= exempt.End() {
			decision[name]++

			// AND THE OPERATOR OPENERS, on both backends: a probe or a
			// maintenance open there would read past the fence or refuse to
			// migrate an unheld ledger, neither of which is what an operator's
			// read does.
			if name != "OpenAdmin" && name != "OpenPostgresAdmin" {
				t.Errorf("%s: openDecisionLedger opens through state.%s, want the operator open of its backend",
					fset.Position(call.Pos()), name)
			}

			if namesAnyRelease(call) {
				t.Errorf("%s: openDecisionLedger names a release, so a standby behind its leader cannot "+
					"read its own instruction", fset.Position(call.Pos()))
			}

			return true
		}

		seen[name]++

		if !namesTheRunningRelease(call) {
			t.Errorf("%s: state.%s is called without state.WithRunningRelease(version.Version()), "+
				"so this open neither refuses a downgrade nor records an upgrade",
				fset.Position(call.Pos()), name)
		}

		return true
	})

	for _, opener := range []string{"Open", "OpenPostgres", "OpenPostgresStandby", "OpenMaintenance",
		"OpenPostgresProbe", "OpenAdmin", "OpenPostgresAdmin", "OpenInspect", "OpenPostgresInspect"} {
		if seen[opener] != 1 {
			t.Errorf("ledger.go calls state.%s %d times outside the decision read, want exactly once",
				opener, seen[opener])
		}
	}

	for _, opener := range []string{"OpenAdmin", "OpenPostgresAdmin"} {
		if decision[opener] != 1 {
			t.Errorf("openDecisionLedger calls state.%s %d times, want exactly once", opener, decision[opener])
		}
	}
}

// namesAnyRelease reports whether an open passes state.WithRunningRelease at all.
func namesAnyRelease(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		if inner, ok := arg.(*ast.CallExpr); ok {
			if p, fn, ok := selectorOf(inner.Fun); ok && p == "state" && fn == "WithRunningRelease" {
				return true
			}
		}
	}

	return false
}

// namesTheRunningRelease reports whether an open passes
// state.WithRunningRelease(version.Version()) itself: WithRunningRelease("")
// names nothing and would satisfy the call's presence.
func namesTheRunningRelease(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		inner, ok := arg.(*ast.CallExpr)
		if !ok {
			continue
		}

		p, fn, ok := selectorOf(inner.Fun)
		if !ok || p != "state" || fn != "WithRunningRelease" || len(inner.Args) != 1 {
			continue
		}

		vcall, ok := inner.Args[0].(*ast.CallExpr)
		if !ok {
			return false
		}

		vp, vf, ok := selectorOf(vcall.Fun)

		return ok && vp == "version" && vf == "Version" && len(vcall.Args) == 0
	}

	return false
}

// selectorOf reads `pkg.Name` out of a call's function expression.
func selectorOf(expr ast.Expr) (string, string, bool) {
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
