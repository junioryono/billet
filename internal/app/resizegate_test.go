package app

import (
	"go/ast"
	"testing"
)

// THE ROUTE ASKS THE GATE THE WIRE CARRIES (#172). Schedule binds the gate the
// ServingWire holds, and TestTheControlPlaneAssemblesStepByStep proves it is
// bound to the scheduler it built; this is the other half, that ServeWire hands
// the node wire's resize route that same gate's resize. Asserted on the source
// because reaching the route for real needs a registered remote node holding a
// launching lease, and a route served with no resizer answers from the store,
// which every other test would still accept.
func TestServeWireHandsTheResizeRouteTheGateItKeeps(t *testing.T) {
	t.Parallel()

	serve := findMethod(t, "Controller", "ServeWire")

	var passed, kept string

	ast.Inspect(serve, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if calleeName(n) != "withResizer" || len(n.Args) != 1 {
				return true
			}

			if sel, ok := n.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "resize" {
				if id, ok := sel.X.(*ast.Ident); ok {
					passed = id.Name
				}
			}
		case *ast.KeyValueExpr:
			if key, ok := n.Key.(*ast.Ident); ok && key.Name == "resizes" {
				if id, ok := n.Value.(*ast.Ident); ok {
					kept = id.Name
				}
			}
		}

		return true
	})

	if passed == "" {
		t.Fatal("ServeWire does not hand the node wire a gate's resize through withResizer")
	}

	if kept != passed {
		t.Errorf("ServeWire hands the route %q's resize and keeps %q on the wire, so Schedule binds a gate "+
			"the route never asks", passed, kept)
	}
}
