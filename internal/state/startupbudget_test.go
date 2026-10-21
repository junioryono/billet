package state

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/importcheck"
)

// A STRETCHED BUDGET IS THE OPEN'S BUDGET. With startupTimeout too short for any
// open, an open of an empty directory fails on its own deadline, and the same
// open given WithStartupBudget migrates the ledger. Not parallel: it shortens
// the package's startupTimeout, which parallel tests read.
func TestAStartupBudgetReplacesTheStartupTimeout(t *testing.T) {
	saved := startupTimeout
	startupTimeout = time.Nanosecond

	t.Cleanup(func() { startupTimeout = saved })

	if db, err := Open(t.Context(), t.TempDir()); err == nil {
		_ = db.Close()

		t.Fatal("an open under a one-nanosecond startup budget succeeded, so this test stages nothing")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the open under a one-nanosecond startup budget = %v, want its deadline", err)
	}

	db, err := Open(t.Context(), t.TempDir(), WithStartupBudget(10*time.Minute))
	if err != nil {
		t.Fatalf("an open given a ten-minute startup budget was held to startupTimeout: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	if db.budget() != 10*time.Minute {
		t.Errorf("the handle keeps a startup budget of %s, want the ten minutes it was opened with "+
			"(a migration at promotion takes it, and its timeout names it)", db.budget())
	}
}

// ONLY THE TEST LEDGER STRETCHES THE BUDGET. WithStartupBudget lengthens a guard
// against a sick disk, so a billet process passing it would open a production
// ledger that waits on that disk for as long as it says. Its one caller is
// ledgertest's template build, as an argument to state.Open; the walk is
// importcheck's, over every non-test file that imports this package, and this
// package's own non-test files.
func TestOnlyTheTestLedgerStretchesTheStartupBudget(t *testing.T) {
	t.Parallel()

	const (
		module = "github.com/junioryono/billet"
		caller = "internal/state/ledgertest/ledgertest.go"
	)

	files := importcheck.ProductionImporters(t, module+"/internal/state")

	own, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list this package's files: %v", err)
	}

	for _, name := range own {
		if !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join("internal", "state", name))
		}
	}

	if !slices.Contains(files, caller) {
		t.Fatalf("%s is not among the files the walk found, so it proves nothing about its callers", caller)
	}

	root := importcheck.ModuleRoot(t)
	fset := token.NewFileSet()

	var callers []string

	inOpen := false

	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		file, err := parser.ParseFile(fset, rel, body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				// The declaration's own name is not a use.
				if node.Recv == nil && node.Name.Name == "WithStartupBudget" {
					return false
				}
			case *ast.CallExpr:
				if rel == caller && isSelector(node.Fun, "state", "Open") {
					for _, arg := range node.Args {
						if call, ok := arg.(*ast.CallExpr); ok && isSelector(call.Fun, "state", "WithStartupBudget") {
							inOpen = true
						}
					}
				}
			case *ast.SelectorExpr:
				if node.Sel.Name == "WithStartupBudget" {
					callers = append(callers, fset.Position(node.Pos()).String())
				}
			case *ast.Ident:
				if node.Name == "WithStartupBudget" && filepath.Dir(rel) == "internal/state" {
					callers = append(callers, fset.Position(node.Pos()).String())
				}
			}

			return true
		})
	}

	if len(callers) != 1 || !strings.HasPrefix(callers[0], caller+":") {
		t.Errorf("WithStartupBudget is used at %v; its one use is %s's template build", callers, caller)
	}

	if !inOpen {
		t.Errorf("%s does not pass WithStartupBudget to state.Open, so the template is migrated under "+
			"the production budget", caller)
	}
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}

	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == pkg
}
