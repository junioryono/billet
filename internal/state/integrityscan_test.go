package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
)

// readinessMargin is what the server unit's start bound must leave beyond an
// open's own budgets: the process starting, the identity and the listeners
// before it reports ready.
const readinessMargin = 10 * time.Second

// THE WHOLE OPEN FITS THE SERVER UNIT'S START BOUND.
//
// The unit is Type=notify with TimeoutStartSec, so systemd kills a control
// plane still opening past it whatever billet's own budgets say. The open is
// the scan's bound plus ONE startup budget, because the startup budget is
// paused for the scan rather than restarted after it.
func TestTheIntegrityScanFitsTheServerUnitsStartBound(t *testing.T) {
	if integrityScanBudget+defaultStartupTimeout+readinessMargin > deploy.UnitStartTimeout {
		t.Errorf("the integrity scan may take %s and the rest of the open %s, which with %s to "+
			"report ready is past the server unit's %s start bound", integrityScanBudget,
			defaultStartupTimeout, readinessMargin, deploy.UnitStartTimeout)
	}

	if integrityScanBudget <= 55*time.Second {
		t.Errorf("the integrity scan's bound %s does not cover the 55s cold read the reference "+
			"ledger measured on 2026-10-17", integrityScanBudget)
	}
}

// THE SCAN IS NOT BOUND BY THE STARTUP BUDGET, WHICH IS PAUSED FOR IT.
//
// A cold quick_check of a large ledger cannot be produced in a unit test, so
// the open path is asserted structurally: openDir hands the backend's scan the
// caller's context rather than the one carrying startupTimeout, measures what
// startup had spent before it, and resumes from the caller's context with
// startupTimeout less that; the SQLite scan bounds the caller's context with
// integrityScanBudget and reads through the bounded one.
func TestTheIntegrityScanRunsOutsideTheStartupBudget(t *testing.T) {
	fset := token.NewFileSet()

	open := funcDecl(t, fset, "state.go", "openDir")
	if open == nil {
		t.Fatal("state.go no longer declares openDir, the open path this test reads")
	}

	var scan, resumed token.Pos

	ast.Inspect(open.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "integrityCheck" || len(node.Args) == 0 {
				return true
			}

			if exprName(node.Args[0]) != "ctx" {
				t.Errorf("openDir scans under %s at %s; the scan must run under the caller's ctx, "+
					"outside the startup budget", exprName(node.Args[0]), fset.Position(node.Pos()))
			}

			scan = node.Pos()
		case *ast.AssignStmt:
			if !scan.IsValid() || node.Pos() < scan || resumed.IsValid() ||
				len(node.Lhs) != 2 || exprName(node.Lhs[0]) != "startupCtx" || len(node.Rhs) != 1 {
				return true
			}

			resumed = node.Pos()

			call, ok := node.Rhs[0].(*ast.CallExpr)
			if !ok || exprName(call.Fun) != "context.WithTimeout" || len(call.Args) != 2 {
				t.Errorf("the startup budget after the scan at %s is not context.WithTimeout",
					fset.Position(node.Pos()))

				return true
			}

			if exprName(call.Args[0]) != "ctx" {
				t.Errorf("the startup budget after the scan derives from %s; deriving from the "+
					"first startup context resumes one that may already have expired",
					exprName(call.Args[0]))
			}

			left, ok := call.Args[1].(*ast.BinaryExpr)
			if !ok || left.Op != token.SUB || exprName(left.X) != "startupTimeout" {
				t.Errorf("the startup budget after the scan is not startupTimeout less what was " +
					"spent before it; restarting it whole lets an open outlast the unit's start bound")
			}
		}

		return true
	})

	if !scan.IsValid() {
		t.Fatal("openDir no longer runs the integrity scan")
	}

	if !resumed.IsValid() {
		t.Error("openDir does not resume the startup budget after the scan")
	}

	check := funcDecl(t, fset, "sqlitebackend.go", "integrityCheck")
	if check == nil {
		t.Fatal("sqlitebackend.go no longer declares integrityCheck")
	}

	first, ok := check.Body.List[0].(*ast.AssignStmt)
	if !ok || len(first.Lhs) != 2 || exprName(first.Lhs[0]) != "ctx" || len(first.Rhs) != 1 {
		t.Fatal("the SQLite scan's first statement does not rebind ctx to a bounded context")
	}

	call, ok := first.Rhs[0].(*ast.CallExpr)
	if !ok || exprName(call.Fun) != "context.WithTimeout" || len(call.Args) != 2 ||
		exprName(call.Args[0]) != "ctx" || exprName(call.Args[1]) != "integrityScanBudget" {
		t.Error("the SQLite scan's first statement is not ctx, cancel := context.WithTimeout(ctx, integrityScanBudget)")
	}

	reads := false

	ast.Inspect(check.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok && len(c.Args) > 0 {
			if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "QueryRowContext" {
				reads = exprName(c.Args[0]) == "ctx"
			}
		}

		return true
	})

	if !reads {
		t.Error("the SQLite scan does not read through its bounded ctx")
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
