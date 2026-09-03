package node

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// A DISPATCHED UPDATER IS REAPED, NOT RELEASED.
//
// Process.Release drops Go's handle on a child; it does not reparent it. The node
// stays the parent, so an updater that exits leaves a zombie until the node
// itself exits — which the original reasoning assumed was imminent, because a
// SUCCESSFUL updater stops this very service. A REFUSED one does not: the node
// keeps running, the rollout retries every few minutes, and the process table
// fills with the corpses of updaters that declined.
//
// A STRUCTURAL TEST BECAUSE THE STATE IS INVISIBLE FROM IN HERE. Whether a child
// was reaped is a property of the process table, not of anything this package
// returns, and the portable ways to ask — scanning for a zombie whose parent is
// this test binary, or racing Wait4 against the goroutine that is reaping — are
// either platform-specific or nondeterministic. What can be asserted exactly is
// that this file does not call Release, which is the mistake itself.
//
// The behavioural half is covered elsewhere: TestTheDispatchReportsAnUpdaterThat*
// run real detached children through this path, so waiting cannot have broken the
// detachment those depend on.
func TestTheUpdaterIsReapedRatherThanReleased(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "upgrade.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse upgrade.go: %v", err)
	}

	// SCOPED TO THE FUNCTION IT NAMES, and scanning the whole file was the first
	// version: any `Wait` in any goroutine anywhere satisfied it, and any unrelated
	// `Release` failed it. Neither says anything about the dispatch.
	var dispatch *ast.FuncDecl

	for _, decl := range file.Decls {
		d, ok := decl.(*ast.FuncDecl)
		if !ok || d.Name.Name != "StartUpgrade" || d.Recv == nil {
			continue
		}

		dispatch = d
	}

	if dispatch == nil {
		t.Fatal("ExecUpgrader.StartUpgrade is gone; this gate is watching a name that moved")

		// UNREACHABLE, AND SAID SO. t.Fatal ends the goroutine, but staticcheck
		// does not model that here and reports the dereference below as a
		// possible nil one -- which is a finding nobody can act on and a gate
		// people learn to ignore.
		return
	}

	var released, reaped bool

	ast.Inspect(dispatch.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Release" {
				released = true
			}
		}

		// THE REAPING IS ASSERTED TOO, and asserting only the absence of Release was
		// the first version — which passed just as happily with BOTH calls deleted,
		// leaving nothing to reap the child at all.
		//
		// It must be a `go` statement: waiting inline would hold the node's single
		// command slot for the length of an unbounded drain, which is the thing the
		// detachment exists to avoid.
		if launch, ok := n.(*ast.GoStmt); ok {
			ast.Inspect(launch, func(inner ast.Node) bool {
				if call, ok := inner.(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Wait" {
						reaped = true
					}
				}

				return true
			})
		}

		return true
	})

	if released {
		t.Error("the dispatch calls Process.Release, which drops Go's handle on the " +
			"updater without reparenting it; a refused updater then stays a zombie for " +
			"as long as this node runs, and the rollout retries every few minutes")
	}

	if !reaped {
		t.Error("nothing in the dispatch waits on the updater from a goroutine, so a " +
			"refused one is never reaped; waiting inline is not the alternative, because " +
			"that holds the node's command slot for the length of an unbounded drain")
	}
}
