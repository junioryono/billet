package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

// The runtime recorder checks admission separately from the boundary. This guard
// makes a new persistence call, or removal of its boundary, visible to it.
func TestRetirementPersistenceCallsHaveStepBoundaries(t *testing.T) {
	files, err := filepath.Glob("serverretire*.go")
	mustOK(t, err)
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		set := token.NewFileSet()
		file, err := parser.ParseFile(set, path, nil, 0)
		mustOK(t, err)
		ast.Inspect(file, func(node ast.Node) bool {
			block, ok := node.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for index, statement := range block.List {
				call := retireStatementCall(statement)
				if call == nil || !retirePersistenceCall(call) {
					continue
				}
				checked++
				var previous *ast.CallExpr
				if index > 0 {
					previous = retireStatementCall(block.List[index-1])
				}
				if previous == nil || retireCallName(previous) != "noteRetireMutation" {
					t.Errorf("%s: persistence call %s has no immediately preceding step boundary", set.Position(call.Pos()), retireCallName(call))
				}
			}
			return true
		})
	}
	if checked < 25 {
		t.Fatalf("persistence inventory unexpectedly small: %d", checked)
	}
}

func retireStatementCall(statement ast.Stmt) *ast.CallExpr {
	switch statement := statement.(type) {
	case *ast.IfStmt:
		if statement.Init != nil {
			return retireStatementCall(statement.Init)
		}
	case *ast.AssignStmt:
		if len(statement.Rhs) == 1 {
			if call, ok := statement.Rhs[0].(*ast.CallExpr); ok {
				return call
			}
		}
	case *ast.ExprStmt:
		if call, ok := statement.X.(*ast.CallExpr); ok {
			return call
		}
	}
	return nil
}

func retireCallName(call *ast.CallExpr) string {
	switch function := call.Fun.(type) {
	case *ast.Ident:
		return function.Name
	case *ast.SelectorExpr:
		if receiver, ok := function.X.(*ast.Ident); ok {
			return receiver.Name + "." + function.Sel.Name
		}
	}
	return ""
}

func retirePersistenceCall(call *ast.CallExpr) bool {
	switch retireCallName(call) {
	case "j.Write", "next.Write", "retirement.WriteStatus", "retirement.WriteStage", "retirement.EnsureRetiredDir",
		"rewriteGuardRecord", "writeGuardRecordAt", "syncDirFD", "db.CompleteRetirement",
		"db.ReserveRetirement", "db.ReleaseRetirement", "db.AdvanceRetirementToIntent", "os.Rename",
		"os.WriteFile", "os.Mkdir", "os.MkdirAll", "os.Chmod", "os.Chown", "os.Create", "os.CreateTemp", "os.RemoveAll":
		return true
	case "os.Remove":
		if len(call.Args) != 1 {
			return false
		}
		path, ok := call.Args[0].(*ast.SelectorExpr)
		return ok && path.Sel.Name == "configPath"
	default:
		return false
	}
}

// Inject at the return from a wait, so a cached entry verdict permits the next
// step. This runs through the command and refuses before any later boundary.
func TestRetirementReadmitsAfterLockAndLedgerWaits(t *testing.T) {
	for _, wait := range []string{"global lock", "identity lock", "ledger open", "completion ledger open"} {
		t.Run(wait, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			saved := retireMutationEvent
			armed := false
			retireMutationEvent = func(event, path string) {
				if !armed && event == "wait" && path == wait {
					armed = true
					armRetirePublicationWatcher(t, f, retirement.RetiredDir())
					return
				}
				if armed && event != "admission" && event != "wait" {
					t.Fatalf("step %s ran after %s armed a watcher", event, wait)
				}
			}
			t.Cleanup(func() { retireMutationEvent = saved })
			out, code := f.request(t, f.input(t, nil))
			if !armed || code != exitUnknown || !strings.Contains(out, "TriggeredBy") {
				t.Fatalf("wait=%s armed=%v: %s", wait, armed, out)
			}
		})
	}
}

func TestRetirementConfigAdmitsStepsRatherThanSyscalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")
	writeFile(t, path, "original", 0o600)
	calls := 0
	mustOK(t, installRetireConfig(path, []byte("replacement"), func(staged string) error {
		calls++
		if mustRead(t, path) != "original" {
			t.Fatal("rewrite crossed its admission before the original was checked")
		}
		if calls == 1 {
			if staged != "" {
				t.Fatal("the first step already created its temporary")
			}
		} else if staged == "" || mustRead(t, staged) != "replacement" {
			t.Fatal("the second step did not follow the completed temporary write")
		}
		return nil
	}))
	if calls != 2 || mustRead(t, path) != "replacement" {
		t.Fatalf("rewrite must admit preparation and post-flush rename: calls=%d", calls)
	}
}
