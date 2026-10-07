package main

import (
	"go/ast"
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
// the wrong stream here would print the operator's output somewhere else.
func TestMainHandsCommandsTheProcessStreams(t *testing.T) {
	t.Parallel()

	want := map[string]string{"Stdout": "Stdout", "Stderr": "Stderr", "Stdin": "Stdin", "Getenv": "Getenv"}
	found := map[string]bool{}

	ast.Inspect(findFunc(t, "main").Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}

		if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Env" {
			return true
		}

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

		return true
	})

	for key := range want {
		if !found[key] {
			t.Errorf("main does not hand commands os.%s as the env's %s", want[key], key)
		}
	}
}
