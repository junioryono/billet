package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// OWNER, THEN SYNC, THEN RENAME, ON ONE DESCRIPTOR. An owner set after the sync is
// not durable with the bytes it belongs to, and one set by name is set on whatever
// the name points at. The recorder in the ownership tests sees which file was
// given away and to whom; it cannot see when, so the order is read from the
// source: the chown precedes the Sync, the Sync precedes the Rename, and no
// by-name Chown or Chmod remains in the copy.
func TestTheStagedCopySetsItsOwnerBeforeItSyncsAndRenames(t *testing.T) {
	t.Parallel()

	file, err := parser.ParseFile(token.NewFileSet(), "hostupgrade.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var copyFn *ast.FuncDecl

	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "copyFileOwnedBy" {
			copyFn = fn
		}
	}

	if copyFn == nil {
		t.Fatal("copyFileOwnedBy was not found in hostupgrade.go")
	}

	var chown, sync, rename token.Pos

	ast.Inspect(copyFn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == "fchownLike" {
				chown = call.Pos()
			}
		case *ast.SelectorExpr:
			pkg, isIdent := fn.X.(*ast.Ident)
			onOS := isIdent && pkg.Name == "os"

			switch {
			case fn.Sel.Name == "Sync":
				sync = call.Pos()
			case onOS && fn.Sel.Name == "Rename":
				rename = call.Pos()
			case onOS && (fn.Sel.Name == "Chown" || fn.Sel.Name == "Chmod" || fn.Sel.Name == "WriteFile"):
				t.Errorf("copyFileOwnedBy still touches the staging file by name through os.%s",
					fn.Sel.Name)
			}
		}

		return true
	})

	switch {
	case chown == token.NoPos || sync == token.NoPos || rename == token.NoPos:
		t.Fatalf("the anchors moved: fchownLike at %v, Sync at %v, os.Rename at %v", chown, sync, rename)
	case chown >= sync || sync >= rename:
		t.Fatalf("the owner must be set (%v) before the sync (%v), and the sync before the rename "+
			"(%v)", chown, sync, rename)
	}
}
