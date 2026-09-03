package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ONLY forceDestroy MAY ASK FOR RUNNING WORK TO BE DESTROYED.
//
// `destroyAll(includeRunning: true)` is the one call in billet that fails a build
// somebody is waiting on, and every other teardown path deliberately passes
// false: a shutdown, a drain that outran its reporting threshold, a second
// signal, a systemd TimeoutStopSec, a failed rollout, a lost leadership epoch.
// The issue's requirement is that none of those may reach it implicitly.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS A NEW CALLER, not a wrong one. Nothing
// in the type system distinguishes the two booleans, so the next person adding a
// teardown path can pass true with no test failing anywhere — the fleet would
// keep working, and the loss would be somebody's build during the next restart.
// This asserts the call GRAPH rather than behaviour, which is the only thing that
// can see a call site that does not exist yet.
//
// It scans the non-test sources so a test may stage the destructive pass
// directly; what it protects is production reachability.
func TestOnlyTheForceOperationDestroysRunningWork(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	callers := map[string][]string{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "destroyAll" || len(call.Args) < 2 {
					return true
				}

				// THE LITERAL, and anything that is not one counts. A caller passing
				// a variable is a caller whose behaviour this test cannot read, which
				// is exactly the case that must be looked at by a person rather than
				// waved through.
				flag, isLiteral := call.Args[1].(*ast.Ident)
				if isLiteral && flag.Name == "false" {
					return true
				}

				where := fset.Position(call.Pos())
				callers[fn.Name.Name] = append(callers[fn.Name.Name],
					where.Filename+":"+itoa(where.Line))

				return true
			})
		}
	}

	delete(callers, "forceDestroy")

	for fn, sites := range callers {
		t.Errorf("%s calls destroyAll with something other than a literal false at %s.\n"+
			"Destroying compute that is still running a job fails a build GitHub will "+
			"NOT requeue, so it may only happen through an explicit, durable, attributed "+
			"operator instruction — which is what forceDestroy carries out. If this is a "+
			"new operator-authorised path, extend this test deliberately; if it is a "+
			"shutdown, a drain, a timeout or a rollback, pass false.",
			fn, strings.Join(sites, ", "))
	}
}

// itoa avoids pulling strconv in for one line of test diagnostics.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte

	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}
