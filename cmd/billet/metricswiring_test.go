package main

import (
	"go/ast"
	"testing"
)

// EACH ROLE SERVES ITS OWN BLOCK'S METRICS, AFTER ITS PROBE AND, ON THE SERVER,
// BEFORE THE CLAIM. A probe runs beside the service that holds the port, so an
// endpoint started above the probe branch fails every upgrade; one started below
// the claim leaves a standby, the process an operator most wants to see waiting,
// unscrapable. The call sites cannot be reached without a ledger, a GitHub and a
// provider, so the order is read from the source.
func TestEachRoleServesItsMetricsAfterItsProbe(t *testing.T) {
	for _, c := range []struct {
		fn, role, block, before string
	}{
		{fn: "runServer", role: "server", block: "Server", before: "BecomeController"},
		{fn: "cmdNode", role: "node", block: "Node", before: "Run"},
	} {
		t.Run(c.fn, func(t *testing.T) {
			body := findFunc(t, c.fn).Body.List

			probe, serve, after := -1, -1, -1

			for i, stmt := range body {
				if ifs, ok := stmt.(*ast.IfStmt); ok && isFlagRef(ifs.Cond, "upgradeProbe") {
					probe = i
				}

				ast.Inspect(stmt, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}

					if isServeMetrics(call, c.role, c.block) && serve < 0 {
						serve = i
					}

					if calleeName(call) == c.before && after < 0 {
						after = i
					}

					return true
				})
			}

			if probe < 0 || serve < 0 || after < 0 {
				t.Fatalf("%s lost a statement this test orders: probe %d, metrics %d, %s %d",
					c.fn, probe, serve, c.before, after)
			}

			if serve <= probe {
				t.Errorf("%s serves metrics at statement %d, not after its probe branch (%d)", c.fn, serve, probe)
			}

			if serve >= after {
				t.Errorf("%s serves metrics at statement %d, not before %s (%d)", c.fn, serve, c.before, after)
			}
		})
	}
}

// isServeMetrics recognises app.ServeMetrics(ctx, "<role>", cfg.<Block>.Metrics).
func isServeMetrics(call *ast.CallExpr, role, block string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ServeMetrics" || len(call.Args) != 3 {
		return false
	}

	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "app" {
		return false
	}

	lit, ok := call.Args[1].(*ast.BasicLit)
	if !ok || lit.Value != `"`+role+`"` {
		return false
	}

	field, ok := call.Args[2].(*ast.SelectorExpr)
	if !ok || field.Sel.Name != "Metrics" {
		return false
	}

	inner, ok := field.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != block {
		return false
	}

	cfg, ok := inner.X.(*ast.Ident)

	return ok && cfg.Name == "cfg"
}
