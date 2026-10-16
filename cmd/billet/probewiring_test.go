package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// BOTH PROBES HAND THEIR WAIT, THEIR FLAG AND THEIR LINE TO host.HoldProbe.
//
// The helper's own tests cannot see whether anything calls it: a probe branch
// put back to `<-ctx.Done()` would leave them green and the fleet hanging at the
// probe step again, which is the outage this guards. The call site cannot be
// reached from a unit test without a ledger and a GitHub, so the source is
// asserted: exactly two upgrade-probe branches that wait, each calling
// host.HoldProbe with the hold flag itself as the third argument and one of the
// two constant lines in the fourth, neither printing nor receiving from a
// channel on its own. Every other upgrade-probe branch is runServer's, which
// returns the probe's verdict and does nothing else.
func TestBothUpgradeProbesHandTheWaitToHoldProbe(t *testing.T) {
	fset := token.NewFileSet()

	var files []*ast.File

	for _, name := range []string{"main.go", "server.go"} {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		files = append(files, file)
	}

	branches := 0

	inspect := func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || !isUpgradeProbeCondition(stmt.Cond) {
			return true
		}

		if isVerdictBranch(stmt) {
			return false
		}

		branches++

		var holds, prints, receives int

		ast.Inspect(stmt.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				if isHoldProbeCall(v) {
					holds++
				}

				if isFmtPrint(v.Fun) {
					prints++
				}
			case *ast.UnaryExpr:
				if v.Op == token.ARROW {
					receives++
				}
			}

			return true
		})

		pos := fset.Position(stmt.Pos())

		if holds != 1 {
			t.Errorf("%s: the upgrade-probe branch calls host.HoldProbe(ctx, env, <hold flag>, <line "+
				"constant>) %d times, want 1", pos, holds)
		}

		if prints != 0 {
			t.Errorf("%s: the upgrade-probe branch prints on its own; the readiness line is "+
				"host.HoldProbe's to print, and only when holding", pos)
		}

		if receives != 0 {
			t.Errorf("%s: the upgrade-probe branch receives from a channel itself; the wait "+
				"belongs to host.HoldProbe, which knows when not to", pos)
		}

		return false
	}

	for _, file := range files {
		ast.Inspect(file, inspect)
	}

	if branches != 2 {
		t.Fatalf("found %d upgrade-probe branches that wait, want 2 (server and node)", branches)
	}
}

// isUpgradeProbeCondition recognises `if upgradeProbe {`, `if *upgradeProbe {`
// and `if run.probe {`.
func isUpgradeProbeCondition(cond ast.Expr) bool {
	return isFlagRef(cond, "upgradeProbe") || isFlagRef(cond, "probe")
}

// isVerdictBranch recognises runServer's `if upgradeProbe { return err }`: the
// probe's exit status is its verdict, returned as it is.
func isVerdictBranch(stmt *ast.IfStmt) bool {
	if len(stmt.Body.List) != 1 {
		return false
	}

	ret, ok := stmt.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}

	_, isName := ret.Results[0].(*ast.Ident)

	return isName
}

// isFlagRef recognises `name`, `*name` and `x.name`.
func isFlagRef(e ast.Expr, name string) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == name
	case *ast.SelectorExpr:
		return v.Sel.Name == name
	case *ast.StarExpr:
		id, ok := v.X.(*ast.Ident)

		return ok && id.Name == name
	}

	return false
}

// isHoldProbeCall recognises host.HoldProbe(ctx, env, holdProbeFlag | *holdProbeFlag,
// <an expression naming host.ServerProbeReadyLine or host.NodeProbeReadyFormat>).
func isHoldProbeCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !isHostSelector(sel, "HoldProbe") || len(call.Args) != 4 {
		return false
	}

	if !isFlagRef(call.Args[2], "holdProbeFlag") && !isFlagRef(call.Args[2], "holdProbe") {
		return false
	}

	found := false

	ast.Inspect(call.Args[3], func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok &&
			(isHostSelector(sel, "ServerProbeReadyLine") || isHostSelector(sel, "NodeProbeReadyFormat")) {
			found = true
		}

		return !found
	})

	return found
}

// isHostSelector recognises host.<name>.
func isHostSelector(sel *ast.SelectorExpr, name string) bool {
	pkg, ok := sel.X.(*ast.Ident)

	return ok && pkg.Name == "host" && sel.Sel.Name == name
}

// isFmtPrint recognises every fmt print: to the process's stdout, or to a
// writer such as the env's.
func isFmtPrint(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	pkg, ok := sel.X.(*ast.Ident)
	name := strings.TrimPrefix(sel.Sel.Name, "F")

	return ok && pkg.Name == "fmt" && (name == "Print" || name == "Println" || name == "Printf")
}
