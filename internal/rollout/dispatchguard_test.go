package rollout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// A PASS THAT COULD NOT SETTLE WHAT IT SAW STARTS NOTHING NEW.
//
// The failure budget is derived from the phases the settle pass wrote, so an
// observation that failed to record leaves it understated — a rollback stuck
// halfway counts as in flight rather than as a failure — and the coordinator
// would then disturb another machine against a tolerance it had already spent.
// The evidence was in this pass's own snapshot; not having been able to write it
// down is a reason to wait for the next pass, not a reason to proceed.
//
// A STRUCTURAL TEST, AND THE ALTERNATIVE WAS A TEST THAT COULD NOT FAIL.
//
// Making a settlement fail on demand means making one Advance fail while every
// read around it still works, and the only seams available are blunt: closing the
// ledger fails `Open` too, so the pass ends before it settles anything and the
// assertion "nothing was dispatched" is satisfied by nothing having run at all.
// That is the shape of a test that passes whatever the code does.
//
// So this asserts the STRUCTURE instead: that the dispatch is reached only under
// a guard on the settle pass's own errors. That is exactly the property, and
// unlike a behavioural test it can also see a second call site that does not
// exist yet — which is the real hazard, since nothing in the types stops one
// being added.
func TestNoHostIsDisturbedByAPassThatCouldNotSettle(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "coordinator.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse coordinator.go: %v", err)
	}

	source, err := os.ReadFile("coordinator.go")
	if err != nil {
		t.Fatalf("read coordinator.go: %v", err)
	}

	var guarded, total int

	ast.Inspect(file, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}

		// THE EXACT CONDITION, not merely one that mentions the word. Matching any
		// condition containing "problems" would accept `len(problems) > 0` — the
		// inverse of the guard — and a name that merely contains the substring.
		if condition(fset, source, stmt.Cond) != "len(problems) == 0" {
			return true
		}

		// Every dispatch inside this guard is accounted for.
		ast.Inspect(stmt.Body, func(inner ast.Node) bool {
			if calls(inner, "dispatchPending") {
				guarded++
			}

			return true
		})

		return true
	})

	ast.Inspect(file, func(n ast.Node) bool {
		if calls(n, "dispatchPending") {
			total++
		}

		return true
	})

	if total == 0 {
		t.Fatal("nothing calls dispatchPending; this gate is watching a name that moved")
	}

	if guarded != total {
		t.Errorf("%d of %d dispatchPending call(s) are inside a guard on the settle pass's "+
			"errors; a dispatch that follows a settlement billet could not write disturbs a "+
			"host against a failure budget it cannot see", guarded, total)
	}
}

// calls reports whether a node is a call to the named function.
func calls(n ast.Node, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}

	return sel.Sel.Name == name
}

// condition renders an if statement's condition as it is written.
func condition(fset *token.FileSet, source []byte, expr ast.Expr) string {
	start := fset.Position(expr.Pos()).Offset
	end := fset.Position(expr.End()).Offset

	if start < 0 || end > len(source) || start >= end {
		return ""
	}

	return strings.TrimSpace(string(source[start:end]))
}
