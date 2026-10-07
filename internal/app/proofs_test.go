package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
)

// proofMakers names, for each value the control plane's order is built from,
// the one function allowed to create it, and the fields only that function may
// set.
var proofMakers = map[string]struct {
	maker  string
	fields []string
}{
	"Controller":         {"BecomeController", []string{"cp", "claim", "loops"}},
	"FleetForgotten":     {"ForgetFleet", []string{"forgottenBy"}},
	"AdoptedAuthority":   {"AdoptAuthority", []string{"adoptedBy"}},
	"AuthorityPublished": {"PublishAuthority", []string{"publishedBy"}},
	"ServingWire":        {"ServeWire", []string{"servedBy"}},
	"Scheduler":          {"Schedule", []string{"plane"}},
}

// ONLY THE STEP THAT PROVES A THING MAKES ITS PROOF.
//
// The control plane's order is its types: a Controller comes only from
// BecomeController, the node wire is served only with the two proofs only this
// controller's ForgetFleet and AdoptAuthority make, and scheduling needs the
// proof PublishAuthority makes. That holds while nothing else in this package
// creates one, which no compiler checks: an exported constructor, a literal,
// a new(T), a conversion or a field written in some other function would hand
// out the proof without the step. So this reads the package for all five, and
// for any function but the maker returning one.
func TestOnlyTheProvingStepsMakeTheirProofs(t *testing.T) {
	t.Parallel()

	guardedField := map[string]string{}

	for typ, m := range proofMakers {
		for _, f := range m.fields {
			guardedField[f] = typ
		}
	}

	var violations []string

	for _, fn := range packageFuncs(t) {
		name := fn.Name.Name
		mayMake := func(typ string) bool { return proofMakers[typ].maker == name }

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				if id, ok := x.Type.(*ast.Ident); ok {
					if _, proof := proofMakers[id.Name]; proof && !mayMake(id.Name) {
						violations = append(violations, name+" builds a "+id.Name+" literal")
					}
				}
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					if _, proof := proofMakers[id.Name]; proof {
						violations = append(violations, name+" converts to "+id.Name)
					}

					if id.Name == "new" && len(x.Args) == 1 {
						if arg, ok := x.Args[0].(*ast.Ident); ok {
							if _, proof := proofMakers[arg.Name]; proof && !mayMake(arg.Name) {
								violations = append(violations, name+" allocates a "+arg.Name)
							}
						}
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok {
						if typ, guarded := guardedField[sel.Sel.Name]; guarded && !mayMake(typ) {
							violations = append(violations, name+" sets "+typ+"."+sel.Sel.Name)
						}
					}
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
				if _, proof := proofMakers[id.Name]; proof && !mayMake(id.Name) {
					violations = append(violations, name+" returns a "+id.Name)
				}
			}
		}
	}

	if len(violations) > 0 {
		t.Errorf("a proof is made outside the step that proves it, which hands it out "+
			"without the step: %v", violations)
	}

	// AND EACH MAKER STILL MAKES IT, or the audit above passes on a package that
	// no longer builds the proof at all.
	for typ, m := range proofMakers {
		fn := findFunc(t, m.maker)

		found := false

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.CompositeLit); ok {
				if id, ok := lit.Type.(*ast.Ident); ok && id.Name == typ {
					found = true
				}
			}

			return true
		})

		if !found {
			t.Errorf("%s no longer builds the %s it proves", m.maker, typ)
		}
	}
}

// THE PROOFS DO NOT CONVERT INTO ONE ANOTHER. Two struct types with identical
// fields convert, so AdoptedAuthority(fleet) would let a caller anywhere skip
// adopting the authority with a proof that only says the fleet was forgotten.
func TestTheProofsDoNotConvert(t *testing.T) {
	t.Parallel()

	proofs := []reflect.Type{
		reflect.TypeFor[FleetForgotten](),
		reflect.TypeFor[AdoptedAuthority](),
		reflect.TypeFor[AuthorityPublished](),
	}

	for _, from := range proofs {
		for _, to := range proofs {
			if from != to && from.ConvertibleTo(to) {
				t.Errorf("a %s converts to a %s, so one step's proof stands in for another's", from, to)
			}
		}
	}
}

// A PROOF ANOTHER CONTROLLER MADE, OR NONE, IS REFUSED, AND SO IS A CONTROLLER
// THAT HOLDS NO CLAIM. Omitting a proof does not compile; a zero one does, and
// so does another controller's, and so does a Controller nobody claimed with,
// so every step checks before it touches anything.
func TestAForeignOrZeroProofIsRefused(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	unclaimed := &Controller{}

	if _, err := unclaimed.ForgetFleet(ctx); err == nil {
		t.Error("a Controller holding no claim forgot the fleet")
	}

	if _, err := unclaimed.AdoptAuthority(ctx); err == nil {
		t.Error("a Controller holding no claim adopted the authority")
	}

	// Two controllers that each look as if they hold a claim, built here (inside
	// the package) only to show that one's proofs do not serve the other.
	ours := &Controller{cp: &ControlPlane{}}
	ours.claim.Epoch = 1
	theirs := &Controller{cp: &ControlPlane{}}
	theirs.claim.Epoch = 1

	for name, tc := range map[string]struct {
		fleet   FleetForgotten
		adopted AdoptedAuthority
	}{
		"zero proofs":                   {},
		"another controller's fleet":    {FleetForgotten{forgottenBy: theirs}, AdoptedAuthority{adoptedBy: ours}},
		"another controller's adoption": {FleetForgotten{forgottenBy: ours}, AdoptedAuthority{adoptedBy: theirs}},
	} {
		if wire, err := ours.ServeWire(ctx, tc.fleet, tc.adopted); err == nil || wire != nil {
			t.Errorf("%s: ServeWire served (%v, %v)", name, wire, err)
		}
	}

	if _, err := ours.PublishAuthority(ctx, &ServingWire{servedBy: theirs}); err == nil {
		t.Error("PublishAuthority published for another controller's wire")
	}

	ourWire := &ServingWire{servedBy: ours}

	for name, tc := range map[string]struct {
		wire      *ServingWire
		published AuthorityPublished
	}{
		"no wire":                         {nil, AuthorityPublished{publishedBy: ours}},
		"another controller's wire":       {&ServingWire{servedBy: theirs}, AuthorityPublished{publishedBy: ours}},
		"no publication":                  {ourWire, AuthorityPublished{}},
		"another controller's publishing": {ourWire, AuthorityPublished{publishedBy: theirs}},
	} {
		if sched, err := ours.Schedule(tc.wire, tc.published, ScheduleOptions{}); err == nil || sched != nil {
			t.Errorf("%s: Schedule assembled (%v, %v)", name, sched, err)
		}
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

// findFunc returns the one function or method of this package with name.
func findFunc(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()

	var found *ast.FuncDecl

	for _, fn := range packageFuncs(t) {
		if fn.Name.Name == name {
			if found != nil {
				t.Fatalf("%s is declared more than once in this package", name)
			}

			found = fn
		}
	}

	if found == nil {
		t.Fatalf("%s is not declared in this package", name)
	}

	return found
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
