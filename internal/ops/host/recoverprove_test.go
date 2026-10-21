package host

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE RECOVERY PROVES ITS DATABASE BEFORE IT SEALS. On PostgreSQL the seal's
// open writes, and may migrate, whatever schema the connection reaches by then,
// so runLocalRecover must call deployarchive.ProveRestoredLedger before
// sealRecoveredDeployment. No unit test reaches that call site with a
// connection that moves mid-run; this holds the order in the source.
func TestARecoveryProvesItsDatabaseBeforeItSeals(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "localrecover.go", nil, 0)
	if err != nil {
		t.Fatalf("parse localrecover.go: %v", err)
	}

	var prove, seal token.Pos

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runLocalRecover" {
			continue
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			switch f := call.Fun.(type) {
			case *ast.SelectorExpr:
				if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name == "deployarchive" &&
					f.Sel.Name == "ProveRestoredLedger" && prove == token.NoPos {
					prove = call.Pos()
				}
			case *ast.Ident:
				if f.Name == "sealRecoveredDeployment" && seal == token.NoPos {
					seal = call.Pos()
				}
			}

			return true
		})
	}

	switch {
	case seal == token.NoPos:
		t.Fatal("runLocalRecover no longer calls sealRecoveredDeployment; this test holds the wrong function")
	case prove == token.NoPos:
		t.Fatal("runLocalRecover seals without calling deployarchive.ProveRestoredLedger")
	case prove > seal:
		t.Fatal("runLocalRecover proves the restored ledger's database only after sealing it")
	}
}
