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

	wired, failed := false, false

	ast.Inspect(run.Body, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.KeyValueExpr:
			if key, ok := x.Key.(*ast.Ident); ok && key.Name == "Ended" {
				if sel, ok := x.Value.(*ast.SelectorExpr); ok && sel.Sel.Name == "endNode" {
					wired = true
				}
			}
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "endNode" && len(x.Args) == 2 {
				if arg, ok := x.Args[0].(*ast.SelectorExpr); ok && arg.Sel.Name == "Failed" {
					failed = true
				}
			}
		}

		return true
	})

	if !wired {
		t.Error("Node.Run does not hand the loop n.endNode as its Ended, so no stop's ending is recorded")
	}

	if !failed {
		t.Error("Node.Run does not record a failure when the loop returns an error")
	}
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
