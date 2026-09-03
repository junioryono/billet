package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/junioryono/billet/internal/provenance"
)

// PRESERVING AND RESTORING READ ONE LIST, AND NOTHING ELSE MAY CARRY ITS OWN.
//
// The two halves used to repeat the same four paths, which is a rollback gap with
// no symptom: a path added to the preserve side and not the restore side is saved
// and never put back, and one added the other way round is restored from a copy
// nobody made. Nothing fails until a rollback that needed it — on a host whose
// services are already down.
//
// A STRUCTURAL TEST BECAUSE THE HAZARD IS A SECOND LIST, not a wrong one. Nothing
// in the types stops the next person writing `for _, path := range []string{...}`
// in either function, and a behavioural test would only cover the paths somebody
// remembered to give it.
func TestPreserveAndRestoreReadTheSameList(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "hostupgrade.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse hostupgrade.go: %v", err)
	}

	for _, name := range []string{"PreserveCurrent", "RestorePreserved"} {
		fn := methodNamed(file, name)
		if fn == nil {
			t.Fatalf("%s is gone; this gate is watching a name that moved", name)

			// Unreachable; see internal/node/reap_test.go for why it is written.
			return
		}

		var callsShared, buildsOwn bool

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "preservedPaths" {
					callsShared = true
				}
			}

			// A composite literal of strings inside either function is a second list
			// by definition, whatever it is called.
			if lit, ok := n.(*ast.CompositeLit); ok {
				if arr, ok := lit.Type.(*ast.ArrayType); ok {
					if elem, ok := arr.Elt.(*ast.Ident); ok && elem.Name == "string" {
						buildsOwn = true
					}
				}
			}

			return true
		})

		if !callsShared {
			t.Errorf("%s does not read preservedPaths, so what it acts on can drift from "+
				"what the other half does", name)
		}

		if buildsOwn {
			t.Errorf("%s builds its own list of paths; a path in one half and not the "+
				"other is saved and never restored, and nothing fails until a rollback "+
				"needs it", name)
		}
	}
}

// AND THE PROVENANCE RECORD IS IN THAT LIST.
//
// A rollback that restored the binary and left the NEW record beside it would
// produce exactly the stale-record state the node-side hash refuses — so the host
// would report no manifest at all, and the rollout would lose the one fact it had
// about which bytes that machine is running. Restoring both together is what
// makes a rolled-back host report the manifest it is actually on.
func TestARollbackRestoresTheProvenanceRecord(t *testing.T) {
	var found bool

	for _, path := range preservedPaths() {
		if path == provenance.Path {
			found = true
		}
	}

	if !found {
		t.Errorf("the provenance record is not preserved, so a rollback leaves the new "+
			"record describing the old binary; %v", preservedPaths())
	}
}

// methodNamed finds a method declaration by name.
func methodNamed(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Name.Name == name {
			return fn
		}
	}

	return nil
}
