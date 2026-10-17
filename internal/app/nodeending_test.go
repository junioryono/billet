package app

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeending"
	"github.com/junioryono/billet/internal/state"
)

// readEndings reads the node's ending record, failing the test on any error.
func readEndings(t *testing.T, stateDir string) []nodeending.Entry {
	t.Helper()

	rec, present, err := nodeending.Read(stateDir)
	if err != nil || !present {
		t.Fatalf("the ending record: present %v, err %v", present, err)
	}

	return rec.Entries
}

// AN OPEN NODE IS RECORDED RUNNING UNDER ITS INVOCATION BEFORE ITS PROVIDER
// EXISTS, and Close records an ending for one that never reached its loop's:
// a node that failed, a probe that was idle.
func TestAnOpenNodeIsRecordedAndClosedWithAnEnding(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		probe bool
		role  nodeending.Role
		end   nodeending.Ending
	}{
		{"a node", false, nodeending.RoleNode, nodeending.Failed},
		{"a probe", true, nodeending.RoleProbe, nodeending.Idle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stateDir, lockDir := t.TempDir(), t.TempDir()

			cfg := openNodeConfig(t, stateDir, lockDir)
			cfg.Node.Provider = config.ProviderDocker

			n, err := OpenNode(cfg, NodeOptions{Probe: tc.probe, InvocationID: "inv-7"})
			if err != nil {
				t.Fatalf("OpenNode: %v", err)
			}

			got := readEndings(t, stateDir)
			if len(got) != 1 || got[0].Ending != nodeending.Running || got[0].InvocationID != "inv-7" ||
				got[0].Role != tc.role || got[0].PID != os.Getpid() || !got[0].RunsOnHost {
				t.Fatalf("after OpenNode the record is %+v, want this process running as %s under inv-7", got,
					tc.role)
			}

			if err := n.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			got = readEndings(t, stateDir)
			if len(got) != 1 || got[0].Ending != tc.end || got[0].EndedAt == nil {
				t.Fatalf("after Close the record is %+v, want the entry ended %s", got, tc.end)
			}
		})
	}
}

// THE RECORD IS WRITTEN BEFORE THE PROVIDER IS ASKED FOR: an open the provider
// refuses has still recorded this process, and records it failed.
func TestTheRecordIsWrittenBeforeTheProvider(t *testing.T) {
	t.Parallel()

	stateDir, lockDir := t.TempDir(), t.TempDir()

	_, err := OpenNode(openNodeConfig(t, stateDir, lockDir), NodeOptions{InvocationID: "inv-1"})
	if err == nil || !strings.Contains(err.Error(), "test harness") {
		t.Fatalf("OpenNode over the simulated backend = %v, want the provider's refusal", err)
	}

	got := readEndings(t, stateDir)
	if len(got) != 1 || got[0].Ending != nodeending.Failed || got[0].InvocationID != "inv-1" {
		t.Fatalf("the record is %+v, want this process recorded and ended failed", got)
	}
}

// THE LOOP'S ENDING REACHES THE RECORD: Node.Run hands the loop n.endNode as
// its Ended, and records a failure when the loop returns an error. Asserted on
// the source because a running loop needs a control plane this package's tests
// do not stand up; the loop's own reports are driven in nodeclient.
func TestNodeRunHandsTheLoopTheRecordWriter(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var run *ast.FuncDecl

	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "Run" && fd.Recv != nil {
			run = fd
		}
	}

	if run == nil {
		t.Fatal("node.go has no Node.Run")
	}

	recv := run.Recv.List[0].Names[0].Name

	// isEndNode is `<recv>.endNode`, on Run's own receiver.
	isEndNode := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "endNode" {
			return false
		}

		x, ok := sel.X.(*ast.Ident)

		return ok && x.Name == recv
	}

	// THE LOOP'S CALL, ITS ENDED, AND THE STATEMENT AFTER IT: `err = RunNodeLoop(
	// ..., LoopOptions{..., Ended: n.endNode})` followed at once by `if err !=
	// nil { n.endNode(nodeending.Failed, ...) }` on the error it assigned.
	for i, stmt := range run.Body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) != 1 {
			continue
		}

		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			continue
		}

		if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "RunNodeLoop" {
			continue
		}

		lhs, ok := assign.Lhs[0].(*ast.Ident)
		if !ok {
			t.Fatal("RunNodeLoop's error is not assigned to a name this test can follow")
		}

		errName := lhs.Name

		opts, ok := call.Args[len(call.Args)-1].(*ast.CompositeLit)
		if !ok {
			t.Fatal("RunNodeLoop's options are not a literal this test can read")
		}

		wired := false

		for _, el := range opts.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}

			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Ended" && isEndNode(kv.Value) {
				wired = true
			}
		}

		if !wired {
			t.Errorf("RunNodeLoop is not handed %s.endNode as its Ended, so no stop's ending is recorded", recv)
		}

		if i+1 >= len(run.Body.List) || !failsOnError(run.Body.List[i+1], errName, isEndNode) {
			t.Errorf("the statement after RunNodeLoop is not `if %s != nil { %s.endNode(nodeending.Failed, "+
				"...) }`, so a loop that errs records no ending", errName, recv)
		}

		return
	}

	t.Fatal("Node.Run never assigns RunNodeLoop's error")
}

// failsOnError says stmt is `if <errName> != nil { ...; <endNode>(nodeending.Failed, ...) }`.
func failsOnError(stmt ast.Stmt, errName string, isEndNode func(ast.Expr) bool) bool {
	ifs, ok := stmt.(*ast.IfStmt)
	if !ok || ifs.Init != nil || ifs.Else != nil {
		return false
	}

	cond, ok := ifs.Cond.(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return false
	}

	x, xok := cond.X.(*ast.Ident)
	y, yok := cond.Y.(*ast.Ident)

	if !xok || !yok || x.Name != errName || y.Name != "nil" {
		return false
	}

	for _, s := range ifs.Body.List {
		es, ok := s.(*ast.ExprStmt)
		if !ok {
			continue
		}

		call, ok := es.X.(*ast.CallExpr)
		if !ok || !isEndNode(call.Fun) || len(call.Args) != 2 {
			continue
		}

		if arg, ok := call.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "Failed" {
			return true
		}
	}

	return false
}

// A RECORD THAT CANNOT BE READ REFUSES THE START, before any provider, and
// gives the lock back: a process that cannot record itself must not touch
// compute, because no stop of it could be proved.
func TestAnUnreadableRecordRefusesTheStart(t *testing.T) {
	t.Parallel()

	stateDir, lockDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(nodeending.Path(stateDir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := openNodeConfig(t, stateDir, lockDir)
	cfg.Node.Provider = config.ProviderDocker

	_, err := OpenNode(cfg, NodeOptions{})
	if err == nil || !strings.Contains(err.Error(), "ending record") {
		t.Fatalf("OpenNode over an unreadable record = %v, want the record's refusal", err)
	}

	_, again, err := ClaimNodeDeployment(openNodeConfig(t, stateDir, lockDir), nil)
	if errors.Is(err, state.ErrDeploymentLocked) || err != nil {
		t.Fatalf("a refused open kept the lock: %v", err)
	}

	if err := again.Release(); err != nil {
		t.Errorf("release: %v", err)
	}
}
