package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE REFUSAL OF A SELF-BLIND DOWNGRADE IS WIRED INTO `rollout start`, between the
// downgrade check and the decision being recorded. Proving checkConvergibleDowngrade
// refuses in isolation says nothing about whether the command asks it, which is
// the shape three defects in this repository have already had; this reads the
// command and asserts the call and its place.
func TestRolloutStartRefusesASelfBlindDowngradeBeforeRecordingTheDecision(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "rollout.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var start *ast.FuncDecl

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "cmdRolloutStart" {
			start = fn
		}
	}

	if start == nil {
		t.Fatal("cmdRolloutStart was not found in rollout.go")
	}

	var downgrade, convergible, record token.Pos

	ast.Inspect(start.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fn := call.Fun.(type) {
		case *ast.Ident:
			switch fn.Name {
			case "checkDowngrade":
				downgrade = call.Pos()
			case "checkConvergibleDowngrade":
				convergible = call.Pos()
			}
		case *ast.SelectorExpr:
			if fn.Sel.Name == "Start" {
				record = call.Pos()
			}
		}

		return true
	})

	switch {
	case convergible == token.NoPos:
		t.Fatal("cmdRolloutStart never asks checkConvergibleDowngrade, so a fleet downgrade to a " +
			"release that cannot see itself on the target is recorded and never converges")
	case downgrade == token.NoPos || record == token.NoPos:
		t.Fatalf("the anchors moved: checkDowngrade at %v, Start at %v", downgrade, record)
	case downgrade >= convergible || convergible >= record:
		t.Fatalf("checkConvergibleDowngrade (%v) must follow checkDowngrade (%v) and precede the "+
			"decision being recorded (%v)", convergible, downgrade, record)
	}
}
