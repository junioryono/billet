package state

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE WATERMARK IS RECORDED INSIDE THE CLAIM'S OWN TRANSACTION, never after it.
//
// Recorded after the commit, a failure to record gave the exclusion back with an
// epoch already published on the handle, which checkLeadership then honoured.
// The tests that watch the mark's final value cannot tell the two apart; this
// reads ClaimController and requires the write to be inside the Tx closure that
// binds the deployment and claims, and nowhere else in the function.
func TestTheWatermarkIsRecordedInsideTheClaimTransaction(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "controller.go", nil, 0)
	if err != nil {
		t.Fatalf("parse controller.go: %v", err)
	}

	var claim *ast.FuncDecl

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "ClaimController" {
			claim = fn
		}
	}

	if claim == nil {
		t.Fatal("controller.go has no ClaimController")
	}

	var inside, outside int

	ast.Inspect(claim, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		switch sel.Sel.Name {
		case "raiseReleaseWatermarkIn":
			if withinTx(claim, call) {
				inside++
			} else {
				outside++
			}
		case "enforceReleaseWatermark":
			outside++
		}

		return true
	})

	if inside != 1 || outside != 0 {
		t.Fatalf("ClaimController records the watermark %d time(s) inside its transaction and %d "+
			"outside it; want exactly one inside and none outside", inside, outside)
	}
}

// withinTx reports whether a call sits inside a function literal passed to db.Tx
// within fn.
func withinTx(fn *ast.FuncDecl, target *ast.CallExpr) bool {
	var found bool

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Tx" {
			return true
		}

		for _, arg := range call.Args {
			lit, ok := arg.(*ast.FuncLit)
			if !ok {
				continue
			}

			if target.Pos() >= lit.Pos() && target.End() <= lit.End() {
				found = true
			}
		}

		return true
	})

	return found
}
