package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/junioryono/billet/deploy"
)

// THE SCAN AND THE STARTUP AFTER IT FIT THE SERVER UNIT'S START BOUND.
//
// The unit is Type=notify with TimeoutStartSec, so systemd kills a control
// plane still scanning past it whatever billet's own budget says. A bound that
// outgrew it would trade billet's timeout for systemd's.
func TestTheIntegrityScanFitsTheServerUnitsStartBound(t *testing.T) {
	if integrityScanBudget+defaultStartupTimeout > deploy.UnitStartTimeout {
		t.Errorf("the integrity scan may take %s and the startup after it %s, past the server "+
			"unit's %s start bound", integrityScanBudget, defaultStartupTimeout, deploy.UnitStartTimeout)
	}

	if integrityScanBudget <= defaultStartupTimeout {
		t.Errorf("the integrity scan's bound %s is no longer than the startup budget %s it was "+
			"split from; a cold read of the reference ledger took 55s", integrityScanBudget,
			defaultStartupTimeout)
	}
}

// THE SCAN IS NOT BOUND BY THE STARTUP BUDGET, AND THE BUDGET RESTARTS AFTER IT.
//
// A cold quick_check of a large ledger cannot be produced in a unit test, so
// the open path is asserted structurally: Open hands the backend's scan the
// caller's context rather than the one carrying startupTimeout, and derives a
// fresh startup bound after the scan; the SQLite scan bounds itself with
// integrityScanBudget before it reads.
func TestTheIntegrityScanRunsOutsideTheStartupBudget(t *testing.T) {
	fset := token.NewFileSet()

	open := funcDecl(t, fset, "state.go", "openDir")
	if open == nil {
		t.Fatal("state.go no longer declares the open path this test reads")
	}

	var scan, rebound token.Pos

	ast.Inspect(open.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "integrityCheck" || len(node.Args) == 0 {
				return true
			}

			arg, ok := node.Args[0].(*ast.Ident)
			if !ok || arg.Name != "ctx" {
				t.Errorf("the open path scans under %s at %s; the scan must run under the "+
					"caller's ctx, outside the startup budget", exprName(node.Args[0]),
					fset.Position(node.Pos()))
			}

			scan = node.Pos()
		case *ast.AssignStmt:
			if node.Tok != token.ASSIGN || len(node.Lhs) != 2 || len(node.Rhs) != 1 {
				return true
			}

			lhs, ok := node.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != "startupCtx" {
				return true
			}

			if scan.IsValid() && node.Pos() > scan && !rebound.IsValid() {
				rebound = node.Pos()
			}
		}

		return true
	})

	if !scan.IsValid() {
		t.Fatal("the open path no longer runs the integrity scan")
	}

	if !rebound.IsValid() {
		t.Error("the open path does not derive a fresh startup budget after the scan, so a " +
			"long scan leaves the migrations less than their budget")
	}

	check := funcDecl(t, fset, "sqlitebackend.go", "integrityCheck")
	if check == nil {
		t.Fatal("sqlitebackend.go no longer declares integrityCheck")
	}

	first, ok := check.Body.List[0].(*ast.AssignStmt)
	if !ok || len(first.Rhs) != 1 {
		t.Fatal("the SQLite scan does not bound itself before it reads")
	}

	call, ok := first.Rhs[0].(*ast.CallExpr)
	if !ok || len(call.Args) != 2 || exprName(call.Fun) != "context.WithTimeout" ||
		exprName(call.Args[1]) != "integrityScanBudget" {
		t.Errorf("the SQLite scan's first statement is not context.WithTimeout(ctx, integrityScanBudget)")
	}
}

// funcDecl is the function or method named name in one of this package's files.
func funcDecl(t *testing.T, fset *token.FileSet, file, name string) *ast.FuncDecl {
	t.Helper()

	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd
		}
	}

	return nil
}

// exprName is an identifier or a selector as written.
func exprName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprName(x.X) + "." + x.Sel.Name
	default:
		return "an expression"
	}
}
