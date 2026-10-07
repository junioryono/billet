package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// EVERY CONTROL-PLANE OPEN OF THE LEDGER NAMES THE RUNNING RELEASE.
//
// The release watermark refuses a proved downgrade and lets the control plane
// record a proved upgrade, and both depend on the opener saying which billet it
// is; an open that names nothing gets neither, and nothing at run time notices.
// ledger.go is where this package opens, so every state.Open* call there must
// carry state.WithRunningRelease(version.Version()), and each of the five opens
// the three modes make is required by name, or a count would be satisfied by
// any five. cmd/billet's ledger.go holds its own opens to the same rule.
func TestEveryControlPlaneLedgerOpenNamesTheRunningRelease(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "ledger.go", nil, 0)
	if err != nil {
		t.Fatalf("parse ledger.go: %v", err)
	}

	seen := map[string]int{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		pkg, name, ok := selectorOf(call.Fun)
		if !ok || pkg != "state" || len(name) < 4 || name[:4] != "Open" {
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

	for _, opener := range []string{"Open", "OpenPostgres", "OpenPostgresStandby", "OpenMaintenance", "OpenPostgresProbe"} {
		if seen[opener] != 1 {
			t.Errorf("ledger.go calls state.%s %d times, want exactly once", opener, seen[opener])
		}
	}
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
