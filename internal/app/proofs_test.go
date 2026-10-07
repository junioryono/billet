package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// ONLY THE STEP THAT PROVES A THING MAKES ITS PROOF.
//
// The control plane's order is its types: a Controller comes only from
// BecomeController, and the node wire is served only with the two proofs that
// only this controller's ForgetFleet and AdoptAuthority make. That holds while
// nothing else in this package builds one, which no compiler checks: an exported
// constructor, or a literal in some other function, would hand out the proof
// without the step. So this reads the package: each proof type is built by a
// composite literal in exactly the one function allowed to, and no function
// returns one but those.
func TestOnlyTheProvingStepsMakeTheirProofs(t *testing.T) {
	t.Parallel()

	allowed := map[string]string{
		"Controller":       "BecomeController",
		"FleetForgotten":   "ForgetFleet",
		"AdoptedAuthority": "AdoptAuthority",
		"ServingWire":      "ServeWire",
	}

	built := map[string][]string{}
	returned := map[string][]string{}

	for _, fn := range packageFuncs(t) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}

			if id, ok := lit.Type.(*ast.Ident); ok {
				if _, proof := allowed[id.Name]; proof && len(lit.Elts) > 0 {
					built[id.Name] = append(built[id.Name], fn.Name.Name)
				}
			}

			return true
		})

		if fn.Type.Results == nil {
			continue
		}

		for _, field := range fn.Type.Results.List {
			typ := field.Type
			if star, ok := typ.(*ast.StarExpr); ok {
				typ = star.X
			}

			if id, ok := typ.(*ast.Ident); ok {
				if _, proof := allowed[id.Name]; proof {
					returned[id.Name] = append(returned[id.Name], fn.Name.Name)
				}
			}
		}
	}

	for proof, maker := range allowed {
		if got := built[proof]; len(got) != 1 || got[0] != maker {
			t.Errorf("a %s with its fields set is built in %v, want only in %s: anything else hands "+
				"out the proof without the step", proof, got, maker)
		}

		if got := returned[proof]; len(got) != 1 || got[0] != maker {
			t.Errorf("a %s is returned by %v, want only by %s", proof, got, maker)
		}
	}
}

// A PROOF ANOTHER CONTROLLER MADE, OR NONE, IS REFUSED. Omitting a proof does
// not compile; a zero one does, and so does another controller's, so ServeWire
// and Run check whose they are before they touch anything.
func TestAForeignOrZeroProofIsRefused(t *testing.T) {
	t.Parallel()

	ours, theirs := &Controller{}, &Controller{}

	for name, tc := range map[string]struct {
		fleet   FleetForgotten
		adopted AdoptedAuthority
	}{
		"zero proofs":                   {},
		"another controller's fleet":    {FleetForgotten{by: theirs}, AdoptedAuthority{by: ours}},
		"another controller's adoption": {FleetForgotten{by: ours}, AdoptedAuthority{by: theirs}},
	} {
		if wire, err := ours.ServeWire(t.Context(), tc.fleet, tc.adopted); err == nil || wire != nil {
			t.Errorf("%s: ServeWire served (%v, %v)", name, wire, err)
		}
	}

	if err := ours.Run(t.Context(), &ServingWire{by: theirs}, RunOptions{}); err == nil {
		t.Error("Run scheduled on another controller's wire")
	}

	if err := ours.Run(t.Context(), nil, RunOptions{}); err == nil {
		t.Error("Run scheduled with no wire")
	}
}

// packageFuncs parses this package's non-test sources and returns every
// function and method declaration.
func packageFuncs(t *testing.T) []*ast.FuncDecl {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	var out []*ast.FuncDecl

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				out = append(out, fn)
			}
		}
	}

	return out
}

// findMethod returns the declaration of a method of this package by receiver
// type and name.
func findMethod(t *testing.T, recv, name string) *ast.FuncDecl {
	t.Helper()

	for _, fn := range packageFuncs(t) {
		if fn.Recv == nil || fn.Name.Name != name || len(fn.Recv.List) != 1 {
			continue
		}

		typ := fn.Recv.List[0].Type
		if star, ok := typ.(*ast.StarExpr); ok {
			typ = star.X
		}

		if id, ok := typ.(*ast.Ident); ok && id.Name == recv {
			return fn
		}
	}

	t.Fatalf("(%s).%s is not declared in this package", recv, name)

	return nil
}

// calleeName is the bare function or method name a call invokes.
func calleeName(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	default:
		return ""
	}
}
