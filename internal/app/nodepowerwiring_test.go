package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// NODE.RUN WIRES THE POLICY THE WAY ITS TESTS ASSUME: built with the metrics
// observer, restored on any early return, given to the runner before it is
// built, and run against the runner's own idle answer before the node loop.
// The call sites cannot be reached without a control plane and a provider, so
// the order is read from the source.
func TestNodeRunWiresTheIdlePowerPolicy(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var run *ast.FuncDecl
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "Run" && fn.Recv != nil &&
			types.ExprString(fn.Recv.List[0].Type) == "*Node" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("node.go has no (*Node).Run")
	}

	at := map[string]token.Pos{}
	note := func(name string, pos token.Pos) {
		if _, seen := at[name]; !seen {
			at[name] = pos
		}
	}
	ast.Inspect(run.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.DeferStmt:
			switch types.ExprString(n.Call) {
			case "closeNodePower(ctx, powerCtl)":
				note("defer close", n.Pos())
			case "stopPower()":
				note("defer stop", n.Pos())
			}
		case *ast.CallExpr:
			switch name := types.ExprString(n.Fun); name {
			case "openNodePower":
				if len(n.Args) == 5 && types.ExprString(n.Args[4]) == "n.powered" {
					note(name, n.Pos())
				}
			case "node.WithPower":
				if len(n.Args) == 1 && types.ExprString(n.Args[0]) == "powerCtl" {
					note(name, n.Pos())
				}
			case "runNodePower":
				if len(n.Args) == 3 && answersNotHolding(n.Args[2]) {
					note(name, n.Pos())
				}
			case "startNodeCache", "NewNodeRunner", "RunNodeLoop":
				note(name, n.Pos())
			}
		}

		return true
	})

	order := []string{"openNodePower", "defer close", "startNodeCache", "node.WithPower", "NewNodeRunner",
		"runNodePower", "defer stop", "RunNodeLoop"}
	for i, name := range order {
		if _, ok := at[name]; !ok {
			t.Fatalf("(*Node).Run lost %s, or calls it with other arguments", name)
		}
		if i > 0 && at[order[i-1]] >= at[name] {
			t.Errorf("(*Node).Run has %s before %s", name, order[i-1])
		}
	}
}

// answersNotHolding reports whether e is func() bool { return !runner.Holding() }.
func answersNotHolding(e ast.Expr) bool {
	lit, ok := e.(*ast.FuncLit)
	if !ok || len(lit.Body.List) != 1 {
		return false
	}
	ret, ok := lit.Body.List[0].(*ast.ReturnStmt)

	return ok && len(ret.Results) == 1 && types.ExprString(ret.Results[0]) == "!runner.Holding()"
}

// OPENNODE RESTORES A LEFTOVER BEFORE IT BUILDS ANYTHING THAT CAN REFUSE THE
// START, and only for a node that is not a probe.
func TestOpenNodeRestoresALeftoverBeforeTheProvider(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var restore, open token.Pos
	var guarded bool
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "OpenNode" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if ifs, ok := n.(*ast.IfStmt); ok && types.ExprString(ifs.Cond) == "!opts.Probe" {
				for _, s := range ifs.Body.List {
					if es, ok := s.(*ast.ExprStmt); ok &&
						types.ExprString(es.X) == "restoreLeftoverPower(cfg, sysfsRoot, runtime.GOOS)" {
						guarded, restore = true, es.Pos()
					}
				}
			}
			if call, ok := n.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "n.open" && open == 0 {
				open = call.Pos()
			}

			return true
		})
	}
	if !guarded || open == 0 {
		t.Fatalf("OpenNode lost its leftover restore under !opts.Probe (%v) or its n.open (%v)", guarded, open)
	}
	if restore >= open {
		t.Error("OpenNode builds its client and provider before it restores a leftover cap")
	}
}
