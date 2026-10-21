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

const (
	// budgetDeclaration is the file that declares WithStartupBudget, whose
	// declared name is the one mention of it that is not a use.
	budgetDeclaration = "internal/state/state.go"
	// budgetCaller is the one file allowed to use it.
	budgetCaller = "internal/state/ledgertest/ledgertest.go"
)

// ONLY THE TEST LEDGER STRETCHES THE BUDGET. WithStartupBudget lengthens a guard
// against a sick disk, so a billet process passing it would open a production
// ledger that waits on that disk for as long as it says. Its one use is
// ledgertest's template build, as an argument to state.Open; the walk is
// importcheck's, over every non-test file that imports this package, and this
// package's own non-test files.
func TestOnlyTheTestLedgerStretchesTheStartupBudget(t *testing.T) {
	t.Parallel()

	files := importcheck.ProductionImporters(t, "github.com/junioryono/billet/internal/state")

	own, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list this package's files: %v", err)
	}

	for _, name := range own {
		if !strings.HasSuffix(name, "_test.go") {
			files = append(files, filepath.Join("internal", "state", name))
		}
	}

	if !slices.Contains(files, budgetCaller) || !slices.Contains(files, budgetDeclaration) {
		t.Fatalf("the walk did not find %s and %s, so it proves nothing about the callers", budgetCaller,
			budgetDeclaration)
	}

	root := importcheck.ModuleRoot(t)
	sources := make(map[string][]byte, len(files))

	for _, rel := range files {
		body, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		sources[filepath.ToSlash(rel)] = body
	}

	uses, inOpen := startupBudgetUses(t, sources)

	if len(uses) != 1 || !strings.HasPrefix(uses[0], budgetCaller+":") {
		t.Errorf("WithStartupBudget is named at %v; its one use is %s's template build", uses, budgetCaller)
	}

	if writes := startupBudgetWrites(t, sources); len(writes) != 0 {
		t.Errorf("the startup budget is set at %v, outside WithStartupBudget and openDir's handle", writes)
	}

	if !inOpen {
		t.Errorf("%s does not pass WithStartupBudget to state.Open, so the template is migrated under "+
			"the production budget", budgetCaller)
	}
}

// THE SCAN SEES EVERY WAY TO NAME IT: a wrapper of the same name, whose body the
// first version skipped with its declaration, and a dot import, whose bare name
// it counted only inside this package.
func TestTheStartupBudgetScanSeesAWrapperAndADotImport(t *testing.T) {
	t.Parallel()

	declaration := []byte(`package state

func WithStartupBudget(d time.Duration) OpenOption { return nil }
`)
	caller := []byte(`package ledgertest

import "github.com/junioryono/billet/internal/state"

func build() { state.Open(nil, "", state.WithStartupBudget(templateBudget)) }
`)

	for name, extra := range map[string]string{
		"a wrapper of the same name": `package elsewhere

import "github.com/junioryono/billet/internal/state"

func WithStartupBudget(d time.Duration) state.OpenOption { return state.WithStartupBudget(d) }
`,
		"a dot import": `package elsewhere

import . "github.com/junioryono/billet/internal/state"

func open() { Open(nil, "", WithStartupBudget(time.Hour)) }
`,
		"a method value": `package elsewhere

import "github.com/junioryono/billet/internal/state"

var stretch = state.WithStartupBudget
`,
	} {
		uses, _ := startupBudgetUses(t, map[string][]byte{
			budgetDeclaration:         declaration,
			budgetCaller:              caller,
			"internal/elsewhere/x.go": []byte(extra),
		})

		var elsewhere int

		for _, use := range uses {
			if strings.HasPrefix(use, "internal/elsewhere/x.go:") {
				elsewhere++
			}
		}

		if elsewhere == 0 {
			t.Errorf("%s: the scan found WithStartupBudget only at %v", name, uses)
		}
	}

	uses, inOpen := startupBudgetUses(t, map[string][]byte{budgetDeclaration: declaration, budgetCaller: caller})
	if len(uses) != 1 || !inOpen {
		t.Errorf("the permitted caller alone scans as %v (passed to Open: %v), want its one use", uses, inOpen)
	}
}

// AND EVERY WAY PAST THE OPTION: the field set by assignment or in a literal,
// anywhere in the package but the option's own body and the handle openDir
// builds from the mode.
func TestTheStartupBudgetScanSeesTheFieldSetDirectly(t *testing.T) {
	t.Parallel()

	permitted := []byte(`package state

func WithStartupBudget(d time.Duration) OpenOption {
	return func(m *openMode) { m.startupBudget = d }
}

func openDir() { db := &DB{startupBudget: mode.budget()} }
`)

	if writes := startupBudgetWrites(t, map[string][]byte{budgetDeclaration: permitted}); len(writes) != 0 {
		t.Errorf("the permitted writes scan as %v", writes)
	}

	for name, src := range map[string]string{
		"an assignment":               "package state\n\nfunc promote(db *DB) { db.startupBudget = time.Hour }\n",
		"a mode literal":              "package state\n\nfunc open() { _ = openMode{startupBudget: time.Hour} }\n",
		"a handle literal in openDir": "package state\n\nfunc openDir() { _ = &DB{startupBudget: time.Hour} }\n",
	} {
		if writes := startupBudgetWrites(t, map[string][]byte{"internal/state/other.go": []byte(src)}); len(writes) == 0 {
			t.Errorf("%s of the startup budget was not seen", name)
		}
	}
}

// startupBudgetWrites lists, as file:line:column, every assignment to a
// startupBudget field and every literal that sets one, in the sources under
// internal/state, except WithStartupBudget's own assignment and openDir's
// `startupBudget: mode.budget()`.
func startupBudgetWrites(t *testing.T, sources map[string][]byte) []string {
	t.Helper()

	fset := token.NewFileSet()

	var writes []string

	for rel, body := range sources {
		if filepath.ToSlash(filepath.Dir(rel)) != "internal/state" {
			continue
		}

		file, err := parser.ParseFile(fset, rel, body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.AssignStmt:
					for _, lhs := range node.Lhs {
						sel, ok := lhs.(*ast.SelectorExpr)
						if ok && sel.Sel.Name == "startupBudget" && fd.Name.Name != "WithStartupBudget" {
							writes = append(writes, fset.Position(lhs.Pos()).String())
						}
					}
				case *ast.KeyValueExpr:
					key, ok := node.Key.(*ast.Ident)
					if !ok || key.Name != "startupBudget" {
						return true
					}

					if fd.Name.Name == "openDir" && isMethodCall(node.Value, "mode", "budget") {
						return true
					}

					writes = append(writes, fset.Position(node.Pos()).String())
				}

				return true
			})
		}
	}

	return writes
}

// isMethodCall reports whether expr is recv.name() with no arguments.
func isMethodCall(expr ast.Expr, recv, name string) bool {
	call, ok := expr.(*ast.CallExpr)

	return ok && len(call.Args) == 0 && isSelector(call.Fun, recv, name)
}

// startupBudgetUses lists every place the sources name WithStartupBudget, as
// file:line:column, except its declared name in budgetDeclaration, and reports
// whether budgetCaller passes it to state.Open. Every identifier counts, so a
// selector, a bare name under a dot import, a method value and a same-named
// wrapper are each a use.
func startupBudgetUses(t *testing.T, sources map[string][]byte) ([]string, bool) {
	t.Helper()

	fset := token.NewFileSet()

	var uses []string

	inOpen := false

	for rel, body := range sources {
		file, err := parser.ParseFile(fset, rel, body, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		var declared *ast.Ident

		if rel == budgetDeclaration {
			for _, decl := range file.Decls {
				if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "WithStartupBudget" {
					declared = fd.Name
				}
			}
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if node.Name == "WithStartupBudget" && node != declared {
					uses = append(uses, fset.Position(node.Pos()).String())
				}
			case *ast.CallExpr:
				if rel == budgetCaller && isSelector(node.Fun, "state", "Open") {
					for _, arg := range node.Args {
						if call, ok := arg.(*ast.CallExpr); ok && isSelector(call.Fun, "state", "WithStartupBudget") {
							inOpen = true
						}
					}
				}
			}

			return true
		})
	}

	slices.Sort(uses)

	return uses, inOpen
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}

	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == pkg
}
