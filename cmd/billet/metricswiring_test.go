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

			probe, serve, after, serves := -1, -1, -1, 0

			for i, stmt := range body {
				if ifs, ok := stmt.(*ast.IfStmt); ok && isFlagRef(ifs.Cond, "upgradeProbe") {
					probe = i

					// THE PROBE BRANCH ENDS THE COMMAND, or the probe falls
					// through to the bind below it.
					if last := ifs.Body.List; len(last) == 0 {
						t.Errorf("%s's probe branch is empty", c.fn)
					} else if _, ok := last[len(last)-1].(*ast.ReturnStmt); !ok {
						t.Errorf("%s's probe branch does not end in a return", c.fn)
					}
				}

				ast.Inspect(stmt, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}

					if isServeMetrics(call, c.role, c.block) {
						serves++

						if serve < 0 {
							serve = i
						}
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

			if serves != 1 {
				t.Errorf("%s calls app.ServeMetrics %d times, want once", c.fn, serves)
			}

			// AND WHAT IT STARTED IS CHECKED AND CLOSED: an error stops the
			// command, and the very next statement defers the endpoint's Close.
			if serve+2 >= len(body) || !servesThenChecksThenCloses(body[serve], body[serve+1], body[serve+2]) {
				t.Errorf("%s does not follow app.ServeMetrics with `if err != nil { return err }` and "+
					"`defer <served>.Close(ctx)`", c.fn)
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

// servesThenChecksThenCloses recognises `v, err := app.ServeMetrics(...)`,
// `if err != nil { return err }` and `defer v.Close(ctx)`, in that order.
func servesThenChecksThenCloses(assign, check, closer ast.Stmt) bool {
	as, ok := assign.(*ast.AssignStmt)
	if !ok || len(as.Lhs) != 2 {
		return false
	}

	served, ok1 := as.Lhs[0].(*ast.Ident)
	errName, ok2 := as.Lhs[1].(*ast.Ident)

	if !ok1 || !ok2 || errName.Name != "err" {
		return false
	}

	ifs, ok := check.(*ast.IfStmt)
	if !ok || len(ifs.Body.List) != 1 {
		return false
	}

	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op.String() != "!=" {
		return false
	}

	if x, ok := cond.X.(*ast.Ident); !ok || x.Name != "err" {
		return false
	}

	ret, ok := ifs.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return false
	}

	if r, ok := ret.Results[0].(*ast.Ident); !ok || r.Name != "err" {
		return false
	}

	def, ok := closer.(*ast.DeferStmt)
	if !ok {
		return false
	}

	sel, ok := def.Call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Close" {
		return false
	}

	recv, ok := sel.X.(*ast.Ident)

	return ok && recv.Name == served.Name
}
