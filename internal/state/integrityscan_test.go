package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/junioryono/billet/deploy"
)

// readinessMargin is a floor the server unit's start bound must leave beyond
// an open's own budgets when systemd grants no extension, for the process's
// own start and its report of readiness. Not a measurement.
const readinessMargin = 10 * time.Second

// THE WHOLE OPEN FITS THE SERVER UNIT'S START BOUND WITHOUT AN EXTENSION.
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

	if StartupBound() != integrityScanBudget+startupTimeout {
		t.Errorf("StartupBound is %s, want the scan's bound and the startup budget, %s",
			StartupBound(), integrityScanBudget+startupTimeout)
	}
}

// THE SCAN RUNS UNDER ITS OWN BOUND, AND THE STARTUP BUDGET IS PAUSED FOR IT.
//
// A cold quick_check of a large ledger cannot be produced in a unit test, so
// the open path is asserted structurally. openDir:
//   - takes started before the first startup budget is derived;
//   - measures spent := time.Since(started) before the scan;
//   - scans under scanCtx := context.WithTimeout(ctx, integrityScanBudget),
//     from the caller's context and not the one carrying startupTimeout;
//   - resumes context.WithTimeout(ctx, startupTimeout - spent) after it.
//
// And the SQLite scan reads through the context it is handed, so the open's
// bound reaches the read and `billet check`'s unbounded context does too.
func TestTheIntegrityScanRunsOutsideTheStartupBudget(t *testing.T) {
	fset := token.NewFileSet()

	open := funcDecl(t, fset, "state.go", "openDir")
	if open == nil {
		t.Fatal("state.go no longer declares openDir, the open path this test reads")
	}

	var started, firstBudget, spent, bound, scan, resumed token.Pos

	var budgeted []token.Pos

	ast.Inspect(open.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Rhs) != 1 {
				return true
			}

			call, ok := node.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}

			lhs := exprName(node.Lhs[0])

			switch {
			case lhs == "started" && exprName(call.Fun) == "time.Now":
				started = node.Pos()
			case lhs == "spent" && exprName(call.Fun) == "time.Since" && len(call.Args) == 1 &&
				exprName(call.Args[0]) == "started":
				spent = node.Pos()
			case lhs == "scanCtx" && exprName(call.Fun) == "context.WithTimeout" && len(call.Args) == 2:
				if exprName(call.Args[0]) != "ctx" || exprName(call.Args[1]) != "integrityScanBudget" {
					t.Error("the scan's context is not context.WithTimeout(ctx, integrityScanBudget)")
				}

				bound = node.Pos()
			case lhs == "startupCtx" && exprName(call.Fun) == "context.WithTimeout" && len(call.Args) == 2:
				if !firstBudget.IsValid() {
					firstBudget = node.Pos()

					return true
				}

				if resumed.IsValid() {
					return true
				}

				resumed = node.Pos()

				if exprName(call.Args[0]) != "ctx" {
					t.Errorf("the startup budget after the scan derives from %s; deriving from the "+
						"first startup context resumes one that may already have expired",
						exprName(call.Args[0]))
				}

				left, ok := call.Args[1].(*ast.BinaryExpr)
				if !ok || left.Op != token.SUB || exprName(left.X) != "startupTimeout" ||
					exprName(left.Y) != "spent" {
					t.Error("the startup budget after the scan is not startupTimeout - spent; " +
						"restarting it whole lets an open outlast the unit's start bound")
				}
			}
		case *ast.CallExpr:
			for _, arg := range node.Args {
				if exprName(arg) == "startupCtx" {
					budgeted = append(budgeted, node.Pos())
				}
			}

			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "integrityCheck" || len(node.Args) == 0 {
				return true
			}

			if exprName(node.Args[0]) != "scanCtx" {
				t.Errorf("openDir scans under %s at %s; the scan runs under scanCtx, its own bound",
					exprName(node.Args[0]), fset.Position(node.Pos()))
			}

			scan = node.Pos()
		}

		return true
	})

	for _, step := range []struct {
		what   string
		at     token.Pos
		before token.Pos
	}{
		{"started := time.Now()", started, firstBudget},
		{"the first startup budget", firstBudget, spent},
		{"spent := time.Since(started)", spent, scan},
		{"the scan's bound", bound, scan},
		{"the scan", scan, resumed},
	} {
		if !step.at.IsValid() || !step.before.IsValid() || step.at > step.before {
			t.Errorf("%s is missing or out of order in openDir; the open takes started, derives the "+
				"startup budget, measures spent, bounds and runs the scan, then resumes the budget", step.what)
		}
	}

	// What ran under the first budget must be measured into spent, and what runs
	// after the scan under the resumed one: a step between the two would be
	// either counted twice or not at all.
	var before, after int

	for _, at := range budgeted {
		switch {
		case spent.IsValid() && at < spent:
			before++
		case resumed.IsValid() && at > resumed:
			after++
		default:
			t.Errorf("a step at %s runs under startupCtx between spent and the resumed budget",
				fset.Position(at))
		}
	}

	if before == 0 || after == 0 {
		t.Errorf("openDir runs %d steps under the startup budget before the scan and %d after it; "+
			"the ping and the preparation precede spent and the schema's checks follow the resume",
			before, after)
	}

	check := funcDecl(t, fset, "sqlitebackend.go", "integrityCheck")
	if check == nil {
		t.Fatal("sqlitebackend.go no longer declares integrityCheck")
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
		t.Error("the SQLite scan does not read through the context it is handed")
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
