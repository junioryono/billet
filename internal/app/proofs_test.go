package app

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// appPath is this package's import path, which the type checker names it by.
const appPath = "github.com/junioryono/billet/internal/app"

// proofMakers names, for each value the control plane's order is built from,
// the one function allowed to create it and the operation it must have
// performed, successfully, before it does, as the type checker names the
// function: what is called, not how the call is spelled.
var proofMakers = map[string]struct{ maker, operation string }{
	"Controller":         {"BecomeController", appPath + ".becomeController"},
	"FleetForgotten":     {"ForgetFleet", "(*github.com/junioryono/billet/internal/alloc.Allocator).ForgetEveryNode"},
	"AdoptedAuthority":   {"AdoptAuthority", appPath + ".AdoptSharedAuthority"},
	"AuthorityPublished": {"PublishAuthority", appPath + ".PublishSharedAuthority"},
	"ServingWire":        {"ServeWire", appPath + ".ServeNodeWire"},
	"Scheduler":          {"Schedule", "github.com/junioryono/billet/internal/server.New"},
}

// errorless are the operations that return no error to test: server.New, and
// the publication, which is non-fatal by design.
var errorless = map[string]bool{
	"github.com/junioryono/billet/internal/server.New": true,
	appPath + ".PublishSharedAuthority":                true,
}

// checkedPackage is this package's production files, type-checked.
type checkedPackage struct {
	files []*ast.File
	info  *types.Info
	// structs are the proofs' underlying struct types: an unnamed struct
	// identical to one is assignable to the proof without a conversion.
	structs map[string]*types.Struct
}

var (
	checkedMu   sync.Mutex
	checkedOnce *checkedPackage
)

// checkedApp type-checks this package's production files against its
// dependencies' export data, once per test binary. The type checker, not a
// reading of the syntax, says what each literal, conversion and field is, so an
// alias, an elided element type or a named container cannot hide a proof.
func checkedApp(t *testing.T) *checkedPackage {
	t.Helper()

	checkedMu.Lock()
	defer checkedMu.Unlock()

	if checkedOnce != nil {
		return checkedOnce
	}

	// THE SAME VARIANT AS THIS TEST BINARY, so the build selects the same files
	// and its cache answers. GOFLAGS reaches go list from the environment.
	build := []string{}
	if raceBuild {
		build = append(build, "-race")
	}

	golist := func(args ...string) string {
		out, err := exec.CommandContext(t.Context(), "go", append(append([]string{"list"}, build...), args...)...).Output()
		if err != nil {
			t.Fatalf("go list %v: %v", args, err)
		}

		return string(out)
	}

	exports := map[string]string{}

	for line := range strings.Lines(golist("-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", ".")) {
		path, file, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && file != "" {
			exports[path] = file
		}
	}

	// EXACTLY THE FILES THE BUILD COMPILES, and every production file must be
	// one: a file a build constraint leaves out here is a file this audit would
	// not read, so one is refused until the audit runs per variant.
	var names []string

	const listed = "{{range .GoFiles}}{{.}}\n{{end}}" +
		"{{range .IgnoredGoFiles}}ignored:{{.}}\n{{end}}{{range .CgoFiles}}cgo:{{.}}\n{{end}}"

	for line := range strings.Lines(golist("-f", listed, ".")) {
		name := strings.TrimSpace(line)
		ignored, isIgnored := strings.CutPrefix(name, "ignored:")
		cgo, isCgo := strings.CutPrefix(name, "cgo:")

		switch {
		case isIgnored && !strings.HasSuffix(ignored, "_test.go"):
			t.Fatalf("%s is left out of this build by a constraint, so this audit cannot read it; "+
				"audit each build variant before adding one", ignored)
		case isCgo:
			t.Fatalf("%s uses cgo, which billet does not build with and this audit does not read", cgo)
		case !isIgnored && name != "":
			names = append(names, name)
		}
	}

	fset := token.NewFileSet()

	var files []*ast.File

	for _, name := range names {
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		files = append(files, file)
	}

	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}

	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}

		return os.Open(file)
	})}

	checked, err := conf.Check(appPath, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", appPath, err)
	}

	structs := map[string]*types.Struct{}

	for name := range proofMakers {
		obj := checked.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("the proof %s is not declared in this package", name)
		}

		st, ok := obj.Type().Underlying().(*types.Struct)
		if !ok || st.NumFields() == 0 {
			t.Fatalf("the proof %s is not a struct with fields", name)
		}

		structs[name] = st
	}

	checkedOnce = &checkedPackage{files: files, info: info, structs: structs}

	return checkedOnce
}

// proofOf names the proof typ is, through aliases and one pointer: the proof
// itself, an unnamed struct identical to its underlying type (assignable to it
// with no conversion), a type of this package declared over either, or a type
// parameter whose constraint admits any of them.
func (pkg *checkedPackage) proofOf(typ types.Type) (string, bool) {
	return pkg.proofOfDepth(typ, 0)
}

func (pkg *checkedPackage) proofOfDepth(typ types.Type, depth int) (string, bool) {
	typ = types.Unalias(typ)
	if p, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(p.Elem())
	}

	switch x := typ.(type) {
	case *types.Named:
		if x.Obj().Pkg() == nil || x.Obj().Pkg().Path() != appPath {
			return "", false
		}

		if _, proof := proofMakers[x.Obj().Name()]; proof {
			return x.Obj().Name(), true
		}

		// A TYPE DECLARED OVER A PROOF'S SHAPE converts to it, and through an
		// unnamed struct is assignable to it, so it is one. The depth bounds a
		// type declared over a pointer to itself.
		if depth < 8 {
			return pkg.proofOfDepth(x.Underlying(), depth+1)
		}
	case *types.Struct:
		for name, st := range pkg.structs {
			if types.Identical(x, st) {
				return name, true
			}
		}
	case *types.TypeParam:
		return pkg.admits(x.Constraint())
	}

	return "", false
}

// admits names a proof a constraint's type set includes.
func (pkg *checkedPackage) admits(constraint types.Type) (string, bool) {
	iface, ok := constraint.Underlying().(*types.Interface)
	if !ok {
		return pkg.proofOf(constraint)
	}

	for i := range iface.NumEmbeddeds() {
		embedded := iface.EmbeddedType(i)

		if union, ok := embedded.(*types.Union); ok {
			for j := range union.Len() {
				if name, proof := pkg.proofOf(union.Term(j).Type()); proof {
					return name, true
				}
			}

			continue
		}

		if name, proof := pkg.admits(embedded); proof {
			return name, true
		}
	}

	return "", false
}

// carriedProofs are the proofs typ is or holds: through a pointer, slice,
// array, map or channel, a struct's fields, or a type of this package declared
// over any of those. A proof is a leaf: what it holds is its own business.
func (pkg *checkedPackage) carriedProofs(typ types.Type) []string {
	var out []string

	seen := map[types.Type]bool{}

	var walk func(types.Type)

	walk = func(typ types.Type) {
		typ = types.Unalias(typ)
		if seen[typ] {
			return
		}

		seen[typ] = true

		if name, proof := pkg.proofOf(typ); proof {
			out = append(out, name)

			return
		}

		switch x := typ.(type) {
		case *types.Pointer:
			walk(x.Elem())
		case *types.Slice:
			walk(x.Elem())
		case *types.Array:
			walk(x.Elem())
		case *types.Map:
			walk(x.Key())
			walk(x.Elem())
		case *types.Chan:
			walk(x.Elem())
		case *types.Struct:
			for i := range x.NumFields() {
				walk(x.Field(i).Type())
			}
		case *types.Named:
			// Another package's type cannot hold one of this package's.
			if x.Obj().Pkg() != nil && x.Obj().Pkg().Path() == appPath {
				walk(x.Underlying())
			}
		}
	}

	walk(typ)

	return out
}

// declaringProof names the proof whose own field sel selects, following the
// path of any embedding to the struct that declares it.
func (pkg *checkedPackage) declaringProof(sel *ast.SelectorExpr) (string, bool) {
	s := pkg.info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal {
		return "", false
	}

	typ := s.Recv()
	path := s.Index()

	for _, i := range path[:len(path)-1] {
		if p, ok := types.Unalias(typ).(*types.Pointer); ok {
			typ = p.Elem()
		}

		st, ok := typ.Underlying().(*types.Struct)
		if !ok {
			return "", false
		}

		typ = st.Field(i).Type()
	}

	return pkg.proofOf(typ)
}

// writtenProofs are the proofs whose storage an assigned or addressed
// expression reaches: a proof's field at any depth of its selector chain, or a
// proof's whole value written in place through a pointer, an index or a field.
func (pkg *checkedPackage) writtenProofs(expr ast.Expr) []string {
	var out []string

	if _, ident := ast.Unparen(expr).(*ast.Ident); !ident {
		if _, pointer := types.Unalias(pkg.info.TypeOf(expr)).(*types.Pointer); !pointer {
			if name, proof := pkg.proofOf(pkg.info.TypeOf(expr)); proof {
				out = append(out, name)
			}
		}
	}

	for expr != nil {
		switch x := expr.(type) {
		case *ast.SelectorExpr:
			if name, proof := pkg.declaringProof(x); proof {
				out = append(out, name)
			}

			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		case *ast.StarExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		default:
			expr = nil
		}
	}

	return out
}

// ONLY THE STEP THAT PROVES A THING MAKES ITS PROOF.
//
// The control plane's order is its types: a Controller comes only from
// BecomeController, the node wire is served only with the two proofs only this
// controller's ForgetFleet and AdoptAuthority make, and scheduling needs the
// proof PublishAuthority makes. That holds while nothing else in this package
// creates one, which no compiler checks. So this type-checks every production
// file, package-level initializers included, and holds every way to make one to
// its maker. A proof here is the type itself, an unnamed struct identical to its
// own (assignable to it with no conversion), or a type parameter whose
// constraint admits either; and the ways are a literal of one however its type
// is spelled or elided, new(T), a field of one written or addressed, one
// overwritten in place, a function returning one in any container or struct,
// and a conversion to one anywhere.
//
// IT STOPS AN EDIT, NOT AN ADVERSARY. What it is for is that a refactor, a new
// helper or a convenience constructor cannot hand out a proof without its step
// unnoticed. What it cannot read (reflect, unsafe, a generic helper over any T
// copying a Controller) the steps refuse at run time where they can: a
// Controller and its control plane are bound to the addresses they were made
// at (TestAForeignOrZeroProofIsRefused).
func TestOnlyTheProvingStepsMakeTheirProofs(t *testing.T) {
	t.Parallel()

	pkg := checkedApp(t)

	var violations []string

	report := func(where, what string) { violations = append(violations, where+" "+what) }

	// audit holds node to what where may make. A closure is audited as its own
	// scope, with no maker's permission: a function literal inside a maker runs
	// whenever something calls it, a deferred one after the maker has chosen
	// what to return.
	var audit func(node ast.Node, where string)

	audit = func(node ast.Node, where string) {
		mayMake := func(name string) bool { return proofMakers[name].maker == where }

		var written []ast.Expr

		ast.Inspect(node, func(n ast.Node) bool {
			if lit, ok := n.(*ast.FuncLit); ok && n != node {
				audit(lit, where+"'s closure")

				return false
			}

			switch x := n.(type) {
			case *ast.CompositeLit:
				if name, proof := pkg.proofOf(pkg.info.TypeOf(x)); proof && !mayMake(name) {
					report(where, "builds a "+name)
				}
			case *ast.CallExpr:
				if tv, ok := pkg.info.Types[x.Fun]; ok && tv.IsType() {
					if name, proof := pkg.proofOf(tv.Type); proof {
						report(where, "converts to "+name)
					}
				}

				if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && len(x.Args) >= 1 {
					b, isBuiltin := pkg.info.Uses[id].(*types.Builtin)

					switch {
					case !isBuiltin:
					case b.Name() == "new":
						if name, proof := pkg.proofOf(pkg.info.TypeOf(x.Args[0])); proof && !mayMake(name) {
							report(where, "allocates a "+name)
						}
					case b.Name() == "copy":
						for _, name := range pkg.carriedProofs(pkg.info.TypeOf(x.Args[0])) {
							if !mayMake(name) {
								report(where, "copies over a "+name)
							}
						}
					}
				}
			case *ast.AssignStmt:
				written = append(written, x.Lhs...)
			case *ast.IncDecStmt:
				written = append(written, x.X)
			case *ast.RangeStmt:
				if x.Tok == token.ASSIGN {
					written = append(written, x.Key, x.Value)
				}
			case *ast.UnaryExpr:
				if x.Op == token.AND {
					for _, name := range pkg.writtenProofs(x.X) {
						if !mayMake(name) {
							report(where, "takes the address of a "+name+"'s storage")
						}
					}
				}
			}

			for _, lhs := range written {
				if lhs == nil {
					continue
				}

				for _, name := range pkg.writtenProofs(lhs) {
					if !mayMake(name) {
						report(where, "writes a "+name+"'s storage")
					}
				}
			}

			written = written[:0]

			return true
		})
	}

	for _, file := range pkg.files {
		for _, decl := range file.Decls {
			where := "package scope"

			if fn, ok := decl.(*ast.FuncDecl); ok {
				where = fn.Name.Name

				obj, ok := pkg.info.Defs[fn.Name].(*types.Func)
				if !ok {
					t.Fatalf("the type checker defined no function for %s", where)
				}

				results := obj.Signature().Results()
				for i := range results.Len() {
					for _, name := range pkg.carriedProofs(results.At(i).Type()) {
						if proofMakers[name].maker != where {
							report(where, "returns a "+name)
						}
					}
				}
			}

			audit(decl, where)
		}
	}

	if len(violations) > 0 {
		t.Errorf("a proof is made outside the step that proves it, which hands it out "+
			"without the step: %v", violations)
	}
}

// EACH MAKER DOES ITS STEP BEFORE IT MAKES ITS PROOF. Only one function may make
// each proof; that is worth nothing if the function makes it without the step.
// So each maker has unnamed results, no deferred call and no label, and calls
// its operation (the function the type checker resolves, not one spelled like
// it) exactly once and names it nowhere else, as a statement of its own body
// (never under a branch, in a goroutine, a deferred call or a closure), binds
// the error that call returns and tests exactly `err != nil` on that variable,
// either in the call's own `if` or in the statement after it, in a branch that
// ends in a return carrying an error. Every return up to that test carries no
// proof (nil or a zero literal), nothing before it builds or writes one, and
// the proof is built after it. A maker that hands its construction to a
// closure or a helper is refused too, on purpose: the proof is built in the
// maker's own body, where this test can read the order it is built in.
func TestEachMakerDoesItsStepBeforeItsProof(t *testing.T) {
	t.Parallel()

	pkg := checkedApp(t)

	for typ, m := range proofMakers {
		fn := pkg.function(t, m.maker)

		if problem := pkg.plainMaker(fn); problem != "" {
			t.Errorf("%s %s, so what it returns is not only what its body says", m.maker, problem)

			continue
		}

		calls, refs := pkg.operationCalls(fn.Body, m.operation)
		if len(calls) != 1 || refs != 1 {
			t.Errorf("%s calls %s %d times and names it %d times, want exactly one call and nothing else",
				m.maker, m.operation, len(calls), refs)

			continue
		}

		guardEnd, problem := pkg.guard(fn.Body, calls[0], errorless[m.operation])
		if problem != "" {
			t.Errorf("%s: %s %s, so the %s it makes need not prove it", m.maker, m.operation, problem, typ)

			continue
		}

		var built token.Pos

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.FuncLit:
				return false
			case *ast.ReturnStmt:
				if x.Pos() < guardEnd {
					for _, result := range x.Results {
						if name, proof := pkg.proofOf(pkg.info.TypeOf(result)); proof && !pkg.zeroProof(result) {
							t.Errorf("%s returns a %s before %s has succeeded", m.maker, name, m.operation)
						}
					}
				}
			case *ast.CompositeLit:
				if name, proof := pkg.proofOf(pkg.info.TypeOf(x)); proof && name == typ && len(x.Elts) > 0 {
					if x.Pos() < guardEnd {
						t.Errorf("%s builds its %s before %s has succeeded", m.maker, typ, m.operation)
					} else if !built.IsValid() {
						built = x.Pos()
					}
				}
			case *ast.AssignStmt, *ast.IncDecStmt, *ast.RangeStmt:
				for _, lhs := range assigned(x) {
					for _, name := range pkg.writtenProofs(lhs) {
						if x.Pos() < guardEnd {
							t.Errorf("%s writes a %s before %s has succeeded", m.maker, name, m.operation)
						}
					}
				}
			}

			return true
		})

		if !built.IsValid() {
			t.Errorf("%s no longer builds the %s it proves", m.maker, typ)
		}
	}
}

// assigned are the expressions a statement writes.
func assigned(stmt ast.Node) []ast.Expr {
	switch x := stmt.(type) {
	case *ast.AssignStmt:
		return x.Lhs
	case *ast.IncDecStmt:
		return []ast.Expr{x.X}
	case *ast.RangeStmt:
		if x.Tok == token.ASSIGN {
			return []ast.Expr{x.Key, x.Value}
		}
	}

	return nil
}

// function returns the one function or method of this package named name, as
// type-checked.
func (pkg *checkedPackage) function(t *testing.T, name string) *ast.FuncDecl {
	t.Helper()

	var found *ast.FuncDecl

	for _, file := range pkg.files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name && fn.Body != nil {
				if found != nil {
					t.Fatalf("%s is declared more than once in this package", name)
				}

				found = fn
			}
		}
	}

	if found == nil {
		t.Fatalf("%s is not declared in this package", name)
	}

	return found
}

// guard finds where the maker's test of its operation ends, or says what is
// wrong with how the call is made or tested. The call must be a statement of
// body itself: an expression statement or the right-hand side of an
// assignment, or the assignment in a top-level if's init. Unless the operation
// is errorless, the call's last result must be bound to a variable, and that
// exact variable tested `!= nil` in the call's own if, or in the next
// statement, whose branch ends in a return whose last value is not nil.
func (pkg *checkedPackage) guard(body *ast.BlockStmt, call *ast.CallExpr, errorless bool) (token.Pos, string) {
	at := -1

	for i, stmt := range body.List {
		if stmt.Pos() <= call.Pos() && call.End() <= stmt.End() {
			at = i
		}
	}

	if at < 0 {
		return token.NoPos, "is not called in the body"
	}

	stmt := body.List[at]

	var (
		assign *ast.AssignStmt
		check  *ast.IfStmt
	)

	switch s := stmt.(type) {
	case *ast.ExprStmt:
		if s.X != call {
			return token.NoPos, "is not called as a statement of its own"
		}

		if errorless {
			return s.End(), ""
		}

		return token.NoPos, "is called and its error dropped"
	case *ast.AssignStmt:
		assign = s

		if errorless {
			if len(s.Rhs) != 1 || s.Rhs[0] != call {
				return token.NoPos, "is not the whole of its assignment"
			}

			return s.End(), ""
		}

		if at+1 >= len(body.List) {
			return token.NoPos, "is not tested"
		}

		next, ok := body.List[at+1].(*ast.IfStmt)
		if !ok || next.Init != nil {
			return token.NoPos, "is not tested in the statement after it"
		}

		check = next
	case *ast.IfStmt:
		init, ok := s.Init.(*ast.AssignStmt)
		if !ok || errorless {
			return token.NoPos, "is called under a branch"
		}

		assign, check = init, s
	default:
		return token.NoPos, "is called under a branch, a loop or a deferred statement"
	}

	if len(assign.Rhs) != 1 || assign.Rhs[0] != call {
		return token.NoPos, "is not the whole of its assignment"
	}

	last, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident)
	if !ok || last.Name == "_" {
		return token.NoPos, "has its error dropped"
	}

	errVar := pkg.info.ObjectOf(last)

	cond, ok := ast.Unparen(check.Cond).(*ast.BinaryExpr)
	if !ok || cond.Op != token.NEQ {
		return token.NoPos, "is not tested by exactly `err != nil`"
	}

	tested, ok := ast.Unparen(cond.X).(*ast.Ident)
	if !ok || pkg.info.ObjectOf(tested) != errVar {
		return token.NoPos, "is not tested on the error it returned"
	}

	if !pkg.isNil(cond.Y) {
		return token.NoPos, "is not tested against nil"
	}

	if len(check.Body.List) == 0 {
		return token.NoPos, "is tested by an empty branch"
	}

	ret, ok := check.Body.List[len(check.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return token.NoPos, "is tested by a branch that does not end in a return"
	}

	if pkg.isNil(ret.Results[len(ret.Results)-1]) {
		return token.NoPos, "is tested by a branch that returns no error"
	}

	return check.End(), ""
}

// isNil reports whether expr is the predeclared nil.
func (pkg *checkedPackage) isNil(expr ast.Expr) bool {
	id, ok := ast.Unparen(expr).(*ast.Ident)
	if !ok {
		return false
	}

	_, isNil := pkg.info.ObjectOf(id).(*types.Nil)

	return isNil
}

// zeroProof reports whether a returned proof is nil or a literal with nothing
// set.
func (pkg *checkedPackage) zeroProof(expr ast.Expr) bool {
	if lit, ok := ast.Unparen(expr).(*ast.CompositeLit); ok {
		return len(lit.Elts) == 0
	}

	return pkg.isNil(expr)
}

// operationCalls finds the calls in body whose callee the type checker
// resolves to operation, and counts every reference to it, called or not: a
// method value or a variable holding the function is a second way to run it.
func (pkg *checkedPackage) operationCalls(body *ast.BlockStmt, operation string) ([]*ast.CallExpr, int) {
	names := func(id *ast.Ident) bool {
		fn, ok := pkg.info.Uses[id].(*types.Func)

		return ok && fn.FullName() == operation
	}

	var (
		calls []*ast.CallExpr
		refs  int
	)

	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			if names(x) {
				refs++
			}
		case *ast.CallExpr:
			var callee *ast.Ident

			switch fun := ast.Unparen(x.Fun).(type) {
			case *ast.Ident:
				callee = fun
			case *ast.SelectorExpr:
				callee = fun.Sel
			}

			if callee != nil && names(callee) {
				calls = append(calls, x)
			}
		}

		return true
	})

	return calls, refs
}

// plainMaker says what lets a maker's result differ from what its returns
// say, or "": named results (which a deferred function can rewrite after the
// maker has returned), a deferred call, or a label to jump to.
func (pkg *checkedPackage) plainMaker(fn *ast.FuncDecl) string {
	if fn.Type.Results != nil {
		for _, field := range fn.Type.Results.List {
			if len(field.Names) > 0 {
				return "names its results"
			}
		}
	}

	problem := ""

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			problem = "defers a call"
		case *ast.LabeledStmt:
			problem = "declares a label"
		}

		return problem == ""
	})

	return problem
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
// so every step checks before it touches anything. A copy of a controller, or
// one overwritten with another's value, holds no claim either: the proofs name
// their controller by address, and the value at that address is no longer the
// one that earned them.
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

	// Controllers that each look as if they hold a claim, built here (inside the
	// package) only to show that one's proofs do not serve another.
	ours := lookingClaimed(&ControlPlane{})
	theirs := lookingClaimed(&ControlPlane{})

	copied := *ours
	if copied.held() {
		t.Fatal("a copy of a Controller holds its claim")
	}

	if _, err := copied.ForgetFleet(ctx); err == nil {
		t.Error("a copy of a Controller forgot the fleet")
	}

	overwritten := lookingClaimed(&ControlPlane{})
	earned := FleetForgotten{forgottenBy: overwritten}
	adopted := AdoptedAuthority{adoptedBy: overwritten}
	*overwritten = *theirs

	if overwritten.held() {
		t.Fatal("a Controller overwritten with another's value holds a claim")
	}

	if wire, err := overwritten.ServeWire(ctx, earned, adopted); err == nil || wire != nil {
		t.Errorf("a Controller overwritten with another's value served with the proofs it had earned: (%v, %v)",
			wire, err)
	}

	// AND THE SAME FOR ITS CONTROL PLANE: proofs earned against one plane do not
	// serve the plane written over it.
	replanned := lookingClaimed(&ControlPlane{})
	earned = FleetForgotten{forgottenBy: replanned}
	adopted = AdoptedAuthority{adoptedBy: replanned}
	*replanned.cp = *theirs.cp

	if replanned.held() {
		t.Fatal("a Controller whose control plane was overwritten with another's holds a claim")
	}

	if wire, err := replanned.ServeWire(ctx, earned, adopted); err == nil || wire != nil {
		t.Errorf("a Controller over an overwritten control plane served with the proofs it had earned: (%v, %v)",
			wire, err)
	}

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

// A FAILED ADOPTION MAKES NO PROOF. A deployment that keeps its authority in a
// store adopts it under the authority lock; when that lock cannot be taken,
// AdoptAuthority must return the error and no proof ServeWire would accept, or
// the wire would read (and on an empty directory mint) a rival authority.
func TestAFailedAdoptionMakesNoProof(t *testing.T) {
	t.Parallel()

	refused := errors.New("the authority lock is held")

	cfg := &config.Config{Server: &config.ServerConfig{
		IdentityDir: t.TempDir(),
		Identity: &config.IdentityConfig{
			Backend: config.IdentitySSM,
			AWSSSM:  &config.IdentitySSMConfig{Region: "us-east-1", Prefix: "/billet/test"},
		},
	}}

	locks := 0

	ctl := lookingClaimed(&ControlPlane{cfg: cfg, host: Host{
		AuthorityLock: func(context.Context, string) (func() error, error) {
			locks++

			return nil, refused
		},
	}})

	adopted, err := ctl.AdoptAuthority(t.Context())
	if !errors.Is(err, refused) {
		t.Fatalf("AdoptAuthority = %v, want the lock's refusal", err)
	}

	if locks != 1 {
		t.Errorf("AdoptAuthority asked for the authority lock %d times, want once", locks)
	}

	if adopted != (AdoptedAuthority{}) {
		t.Errorf("a failed adoption returned a proof made by %p", adopted.adoptedBy)
	}
}

// lookingClaimed is a Controller over cp that passes held() without a ledger,
// for the tests that show what a step refuses.
func lookingClaimed(cp *ControlPlane) *Controller {
	cp.self = cp
	ctl := &Controller{cp: cp}
	ctl.claim.Epoch = 1
	ctl.self = ctl

	return ctl
}

// THE CONTROL PLANE ASSEMBLES, EACH STEP'S PROOF OPENING THE NEXT. A real
// control plane over a migrated SQLite ledger and a loopback node wire walks
// every step a server does short of polling GitHub: it opens, takes the claim
// (no standby on SQLite), forgets the fleet, adopts the authority (a no-op with
// no identity store), serves the wire, publishes (likewise), and schedules,
// sending READY=1. A step that stopped producing a usable proof, or refused one
// it made, stops here.
func TestTheControlPlaneAssemblesStepByStep(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen:      "127.0.0.1:0",
		IdentityDir: ledgertest.Dir(t),
		MaxVCPU:     8,
		MaxMemory:   16 * config.GiB,
	}}

	var readies int

	host := Host{
		ServerAccess:  noIdentityAccess,
		AuthorityLock: noIdentityAccess,
		Ready: func() error {
			readies++

			return nil
		},
		Status: func(string) error { return nil },
		Out:    io.Discard,
	}

	cp, err := OpenControlPlane(ctx, cfg, host, nil, ControlPlaneOptions{})
	if err != nil {
		t.Fatalf("OpenControlPlane: %v", err)
	}

	t.Cleanup(func() { _ = cp.Close() })

	stopped := false

	ctl, err := cp.BecomeController(ctx, func() { stopped = true })
	if err != nil {
		t.Fatalf("BecomeController: %v", err)
	}

	defer ctl.Close()

	adopted, err := ctl.AdoptAuthority(ctx)
	if err != nil {
		t.Fatalf("AdoptAuthority: %v", err)
	}

	fleet, err := ctl.ForgetFleet(ctx)
	if err != nil {
		t.Fatalf("ForgetFleet: %v", err)
	}

	wire, err := ctl.ServeWire(ctx, fleet, adopted)
	if err != nil {
		t.Fatalf("ServeWire: %v", err)
	}

	defer wire.Stop()

	if wire.Addr == "" {
		t.Error("the served wire has no address")
	}

	published, err := ctl.PublishAuthority(ctx, wire)
	if err != nil {
		t.Fatalf("PublishAuthority: %v", err)
	}

	scheduler, err := ctl.Schedule(wire, published, ScheduleOptions{})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if scheduler == nil || readies != 1 {
		t.Errorf("Schedule returned %v having sent READY=1 %d times, want a scheduler and once", scheduler, readies)
	}

	if stopped || cp.LeadershipLost() {
		t.Error("an unfenced controller stopped itself")
	}
}

// packageFiles parses this package's non-test sources.
func packageFiles(t *testing.T) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()

	var out []*ast.File

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		out = append(out, file)
	}

	return out
}

// packageFuncs is every function and method declaration of this package's
// non-test sources.
func packageFuncs(t *testing.T) []*ast.FuncDecl {
	t.Helper()

	var out []*ast.FuncDecl

	for _, file := range packageFiles(t) {
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
		if isMethod(fn, recv, name) {
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
