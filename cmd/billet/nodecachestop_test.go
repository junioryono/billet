package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
)

// A HANDOVER LETS THE CACHE FINISH WHAT IT IS SENDING (#374), within what the
// leases can spare. The guests outlive a node that hands over, so a transfer cut at
// the old five seconds fails a job; but nothing renews their leases until the next
// process registers, so the grace stays within a sixth of the TTL. A drain stops
// the listener after its jobs, and keeps the short grace.
func TestTheNodeCacheStopsWithAHandoverGrace(t *testing.T) {
	t.Parallel()

	if got := nodeCacheStopGrace(true); got <= 5*time.Second || got > alloc.DefaultLeaseTTL/6 {
		t.Errorf("a handover gives in-flight cache requests %v, want more than 5s and at most %v",
			got, alloc.DefaultLeaseTTL/6)
	}
	if got := nodeCacheStopGrace(false); got != 5*time.Second {
		t.Errorf("a drain gives in-flight cache requests %v, want 5s", got)
	}
}

// AND THE NODE STARTS ITS CACHE WITH IT, the listener's Shutdown waits that
// long, and the cache serves only once the loop says the node is ready. No run-time test reaches startNodeCache without a provider and a
// listener, so this one reads the source.
func TestTheNodeCacheIsGivenItsStopGrace(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var passed, honoured, shutdown, gated bool

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			key, keyOK := node.Key.(*ast.Ident)
			value, valueOK := node.Value.(*ast.Ident)
			if keyOK && valueOK && key.Name == "Ready" && value.Name == "serveCache" {
				gated = true
			}
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
			ast.Inspect(node, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok || !namesSelector(call.Fun, "srv", "Shutdown") || len(call.Args) != 1 {
					return true
				}
				if arg, ok := call.Args[0].(*ast.Ident); ok && arg.Name == "shutdownCtx" {
					shutdown = true
				}

				return true
			})
		}

		return true
	})

	if !passed {
		t.Error("the node starts its cache without nodeCacheStopGrace(handOver)")
	}
	if !gated {
		t.Error("the node loop is not given serveCache as Ready, so the cache answers before registration")
	}
	if !honoured || !shutdown {
		t.Error("startNodeCache's srv.Shutdown(shutdownCtx) does not wait stopGrace")
	}
}
