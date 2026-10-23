package app

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"strings"
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

	// EACH STEP IS ONE OF Run's OWN STATEMENTS, or the whole body of a plain
	// `if powerCtl != nil` among them, matched as printed: a step inside another
	// condition, behind an init that shadows a name, or with its result
	// discarded is not the step.
	steps := make([]string, 0, len(run.Body.List))
	for _, stmt := range run.Body.List {
		if ifs, ok := stmt.(*ast.IfStmt); ok && ifs.Init == nil && ifs.Else == nil &&
			types.ExprString(ifs.Cond) == "powerCtl != nil" {
			body := make([]string, 0, len(ifs.Body.List))
			for _, s := range ifs.Body.List {
				body = append(body, render(t, fset, s))
			}
			steps = append(steps, "if powerCtl != nil { "+strings.Join(body, "; ")+" }")

			continue
		}
		steps = append(steps, render(t, fset, stmt))
	}

	want := []string{
		"powerCtl, err := openNodePower(ctx, cfg, sysfsRoot, runtime.GOOS, n.powered)",
		"closePower := func() {}",
		"if powerCtl != nil { closePower = sync.OnceFunc(func() { closeNodePower(ctx, powerCtl) }); defer closePower() }",
		"cacheService, serveCache, stopCache, err := startNodeCache(",
		"if powerCtl != nil { runnerOpts = append(runnerOpts, node.WithPower(powerCtl)) }",
		"runner := NewNodeRunner(n.client, cfg.Node.Name, p, slog.Default(), runnerOpts...)",
		"if powerCtl != nil { stopPower := runNodePower(ctx, powerCtl, runner.Idle, closePower); defer stopPower() }",
		// THE LOOP RUNS THE RUNNER THE POLICY ASKS, or Idle never sees a recovery.
		"err = RunNodeLoop(ctx, n.client, runner,",
	}
	at := 0
	for _, w := range want {
		found := false
		for ; at < len(steps); at++ {
			if strings.HasPrefix(steps[at], w) {
				found = true

				break
			}
		}
		if !found {
			t.Fatalf("(*Node).Run lacks, in order, the statement %q", w)
		}
	}

	// NOTHING REPLACES WHAT THE STEPS BUILT between them.
	for _, s := range steps {
		for _, rebound := range []string{"runnerOpts = nil", "closePower = func", "runnerOpts = []"} {
			if strings.HasPrefix(s, rebound) {
				t.Errorf("(*Node).Run replaces what the power wiring built: %q", s)
			}
		}
	}
}

// render prints one statement on one line, as the audit compares it.
func render(t *testing.T, fset *token.FileSet, stmt ast.Stmt) string {
	t.Helper()

	var b bytes.Buffer
	if err := format.Node(&b, fset, stmt); err != nil {
		t.Fatal(err)
	}

	return strings.Join(strings.Fields(b.String()), " ")
}

// OPENNODE RESTORES A LEFTOVER BEFORE ANYTHING THAT CAN REFUSE THE START, its
// certificate and its deployment lock included, and only for a node that is not
// a probe.
func TestOpenNodeRestoresALeftoverBeforeTheProvider(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	// TOP-LEVEL STATEMENTS ONLY, by index: a restore nested in a closure, a
	// loop or another branch would come first in the text and never run first.
	var body []ast.Stmt
	for _, d := range file.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "OpenNode" {
			body = fn.Body.List
		}
	}
	calls := func(stmt ast.Stmt, name string) bool {
		found := false
		ast.Inspect(stmt, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && types.ExprString(call.Fun) == name {
				found = true
			}

			return !found
		})

		return found
	}
	first := func(name string) int {
		for i, stmt := range body {
			if calls(stmt, name) {
				return i
			}
		}

		return -1
	}
	restoreAt, restores := -1, 0
	for i, stmt := range body {
		ifs, ok := stmt.(*ast.IfStmt)
		if !ok || ifs.Init != nil || ifs.Else != nil || types.ExprString(ifs.Cond) != "!opts.Probe" ||
			len(ifs.Body.List) != 1 {
			continue
		}
		if es, ok := ifs.Body.List[0].(*ast.ExprStmt); ok &&
			types.ExprString(es.X) == "restoreLeftoverPower(cfg, sysfsRoot, runtime.GOOS, leftover)" {
			restoreAt = i
		}
	}
	for _, stmt := range body {
		if calls(stmt, "restoreLeftoverPower") {
			restores++
		}
	}
	bundle, claim, ending, open := first("NodeBundle"), first("ClaimNodeDeployment"),
		first("beginNodeEnding"), first("n.open")
	if restoreAt < 0 || restores != 1 || bundle < 0 || claim < 0 || ending < 0 || open < 0 {
		t.Fatalf("OpenNode lost its one top-level leftover restore under !opts.Probe (at %d, %d of them), "+
			"its bundle, claim, ending or open (%d, %d, %d, %d)", restoreAt, restores, bundle, claim, ending, open)
	}
	restore := restoreAt
	if restore >= bundle || restore >= claim {
		t.Error("OpenNode reads its certificate or takes its deployment lock before it restores a leftover " +
			"cap, so either refusing leaves the host capped")
	}
	if restore >= ending || restore >= open {
		t.Error("OpenNode records its ending or builds its provider before it restores a leftover cap")
	}
}
