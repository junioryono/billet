package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// A HANDOVER LETS THE CACHE FINISH WHAT IT IS SENDING (#374). The guests outlive a
// node that hands over, so a transfer cut at the old five seconds fails a job; a
// drain stops the listener after its jobs, and keeps the short grace.
func TestTheNodeCacheStopsWithAHandoverGrace(t *testing.T) {
	t.Parallel()

	if got := nodeCacheStopGrace(true); got < time.Minute {
		t.Errorf("a handover gives in-flight cache requests %v, want at least a minute", got)
	}
	if got := nodeCacheStopGrace(false); got != 5*time.Second {
		t.Errorf("a drain gives in-flight cache requests %v, want 5s", got)
	}
}

// AND THE NODE STARTS ITS CACHE WITH IT, and the listener's Shutdown waits that
// long. No run-time test reaches startNodeCache without a provider and a
// listener, so this one reads the source.
func TestTheNodeCacheIsGivenItsStopGrace(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var passed, honoured bool

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			name, ok := node.Fun.(*ast.Ident)
			if !ok || name.Name != "startNodeCache" || len(node.Args) == 0 {
				return true
			}

			grace, ok := node.Args[len(node.Args)-1].(*ast.CallExpr)
			if !ok || len(grace.Args) != 1 {
				return true
			}

			fun, funOK := grace.Fun.(*ast.Ident)
			arg, argOK := grace.Args[0].(*ast.Ident)
			if funOK && argOK && fun.Name == "nodeCacheStopGrace" && arg.Name == "handOver" {
				passed = true
			}
		case *ast.FuncDecl:
			if node.Name.Name != "startNodeCache" {
				return true
			}

			ast.Inspect(node, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok || !namesSelector(call.Fun, "context", "WithTimeout") || len(call.Args) != 2 {
					return true
				}

				if grace, ok := call.Args[1].(*ast.Ident); ok && grace.Name == "stopGrace" {
					honoured = true
				}

				return true
			})
		}

		return true
	})

	if !passed {
		t.Error("the node starts its cache without nodeCacheStopGrace(handOver)")
	}
	if !honoured {
		t.Error("startNodeCache's Shutdown does not wait stopGrace")
	}
}
