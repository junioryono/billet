package main

import (
	"go/ast"
	"testing"
)

// `billet status` reports the host's own guard: cmdStatus hands its env to the
// guard report internal/ops/host tests (TestTheGuardReportNamesTheHostsOwnGuard),
// which reads the upgrade root and never the ledger.
func TestStatusReportsTheHostsOwnGuard(t *testing.T) {
	calls := 0

	ast.Inspect(findFunc(t, "cmdStatus"), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "PrintGuard" {
			return true
		}

		pkg, pkgOK := sel.X.(*ast.Ident)
		arg, argOK := call.Args[0].(*ast.Ident)

		if pkgOK && argOK && pkg.Name == "host" && arg.Name == "env" {
			calls++
		}

		return true
	})

	if calls != 1 {
		t.Errorf("cmdStatus calls host.PrintGuard(env) %d times, want once", calls)
	}
}
