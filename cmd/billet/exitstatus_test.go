package main

import (
	"go/ast"
	"go/token"
	"go/types"
	"testing"

	"github.com/junioryono/billet/internal/cli"
)

// THE RUNNER CHECK ANSWERS WITH THE STATUSES IT DOCUMENTS: 2 when a rebuild is
// due, 3 when GitHub is already refusing the runner. A monitor acts on the
// difference, so the numbers are the contract.
func TestTheRunnerCheckAnswersWithItsDocumentedStatuses(t *testing.T) {
	t.Parallel()

	if got := cli.ExitStatus(errRunnerDue); got != 2 {
		t.Errorf("a due rebuild exits %d, want 2", got)
	}

	if got := cli.ExitStatus(errExpiredRunner); got != 3 {
		t.Errorf("a runner GitHub refuses exits %d, want 3", got)
	}
}

// MAIN HANDS EVERY COMMAND THE PROCESS'S OWN STREAMS AND ENVIRONMENT, and is
// the only place that does: a command writes to its cli.Env, so one built with
// the wrong stream here would print the operator's output somewhere else. The
// env checked is the one cli.Main is handed: a literal, or a variable whose one
// assignment is that literal and which nothing in main changes afterwards.
func TestMainHandsCommandsTheProcessStreams(t *testing.T) {
	t.Parallel()

	body := findFunc(t, "main").Body

	var handed ast.Expr

	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Main" && len(call.Args) > 1 {
			if handed != nil {
				t.Error("main calls cli.Main more than once")
			}

			handed = call.Args[1]
		}

		return true
	})

	if handed == nil {
		t.Fatal("main does not call cli.Main")
	}

	lit, ok := handed.(*ast.CompositeLit)

	if id, isIdent := handed.(*ast.Ident); isIdent {
		assignments := 0

		ast.Inspect(body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					root := lhs
					for {
						sel, isSel := root.(*ast.SelectorExpr)
						if !isSel {
							break
						}

						root = sel.X
					}

					if r, isID := root.(*ast.Ident); !isID || r.Name != id.Name {
						continue
					}

					assignments++

					if l, isLit := x.Rhs[min(i, len(x.Rhs)-1)].(*ast.CompositeLit); isLit && lhs == root {
						lit, ok = l, true
					} else {
						t.Errorf("main changes the env it hands cli.Main: %s", types.ExprString(lhs))
					}
				}
			case *ast.UnaryExpr:
				if r, isID := x.X.(*ast.Ident); isID && r.Name == id.Name && x.Op == token.AND {
					t.Error("main takes the env's address, so something could change it before cli.Main")
				}
			}

			return true
		})

		if assignments != 1 {
			t.Errorf("main assigns the env it hands cli.Main %d times, want once", assignments)
		}
	}

	if !ok {
		t.Fatalf("cli.Main is handed %s, not an env literal main builds", types.ExprString(handed))
	}

	want := map[string]string{"Stdout": "Stdout", "Stderr": "Stderr", "Stdin": "Stdin", "Getenv": "Getenv"}
	found := map[string]bool{}

	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}

		key, keyOK := kv.Key.(*ast.Ident)
		value, valueOK := kv.Value.(*ast.SelectorExpr)

		if keyOK && valueOK && want[key.Name] == value.Sel.Name {
			if pkg, ok := value.X.(*ast.Ident); ok && pkg.Name == "os" {
				found[key.Name] = true
			}
		}
	}

	for _, key := range sortedKeys(want) {
		if !found[key] {
			t.Errorf("main does not hand commands os.%s as the env's %s", want[key], key)
		}
	}
}
