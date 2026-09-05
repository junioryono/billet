package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// BOTH PROBES HAND THEIR WAIT, THEIR FLAG AND THEIR LINE TO holdProbe.
//
// The helper's own tests cannot see whether anything calls it: a probe branch
// put back to `<-ctx.Done()` would leave them green and the fleet hanging at the
// probe step again, which is the outage this guards. The call site cannot be
// reached from a unit test without a ledger and a GitHub, so the source is
// asserted: exactly two upgrade-probe branches, each calling holdProbe with the
// hold flag itself as the second argument and one of the two constant lines in
// the third, neither printing nor receiving from a channel on its own.
func TestBothUpgradeProbesHandTheWaitToHoldProbe(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	branches := 0

	ast.Inspect(file, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || !isUpgradeProbeCondition(stmt.Cond) {
			return true
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
			t.Errorf("%s: the upgrade-probe branch calls holdProbe(ctx, <hold flag>, <line "+
				"constant>) %d times, want 1", pos, holds)
		}

		if prints != 0 {
			t.Errorf("%s: the upgrade-probe branch prints on its own; the readiness line is "+
				"holdProbe's to print, and only when holding", pos)
		}

		if receives != 0 {
			t.Errorf("%s: the upgrade-probe branch receives from a channel itself; the wait "+
				"belongs to holdProbe, which knows when not to", pos)
		}

		return false
	})

	if branches != 2 {
		t.Fatalf("found %d upgrade-probe branches in main.go, want 2 (server and node)", branches)
	}
}

// isUpgradeProbeCondition recognises `if upgradeProbe {` and `if *upgradeProbe {`.
func isUpgradeProbeCondition(cond ast.Expr) bool {
	return isFlagRef(cond, "upgradeProbe")
}

// isFlagRef recognises `name` and `*name`.
func isFlagRef(e ast.Expr, name string) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == name
	case *ast.StarExpr:
		id, ok := v.X.(*ast.Ident)

		return ok && id.Name == name
	}

	return false
}

// isHoldProbeCall recognises holdProbe(ctx, holdProbeFlag | *holdProbeFlag, <an
// expression naming serverProbeReadyLine or nodeProbeReadyFormat>).
func isHoldProbeCall(call *ast.CallExpr) bool {
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "holdProbe" || len(call.Args) != 3 {
		return false
	}

	if !isFlagRef(call.Args[1], "holdProbeFlag") {
		return false
	}

	found := false

	ast.Inspect(call.Args[2], func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok &&
			(id.Name == "serverProbeReadyLine" || id.Name == "nodeProbeReadyFormat") {
			found = true
		}

		return !found
	})

	return found
}

// isFmtPrint recognises fmt.Print, fmt.Println and fmt.Printf.
func isFmtPrint(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	pkg, ok := sel.X.(*ast.Ident)

	return ok && pkg.Name == "fmt" && (sel.Sel.Name == "Print" || sel.Sel.Name == "Println" ||
		sel.Sel.Name == "Printf")
}
