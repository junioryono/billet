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
// performed, successfully, before it does.
var proofMakers = map[string]struct{ maker, operation string }{
	"Controller":         {"BecomeController", "becomeController"},
	"FleetForgotten":     {"ForgetFleet", "ForgetEveryNode"},
	"AdoptedAuthority":   {"AdoptAuthority", "AdoptSharedAuthority"},
	"AuthorityPublished": {"PublishAuthority", "PublishSharedAuthority"},
	"ServingWire":        {"ServeWire", "ServeNodeWire"},
	"Scheduler":          {"Schedule", "server.New"},
}

// checkedPackage is this package's production files, type-checked.
type checkedPackage struct {
	files []*ast.File
	types *types.Package
	info  *types.Info
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

	// THE SAME VARIANT AS THIS TEST BINARY, so its build cache answers.
	args := []string{"list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}
	if raceBuild {
		args = append(args, "-race")
	}

	out, err := exec.CommandContext(t.Context(), "go", append(args, ".")...).Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}

	exports := map[string]string{}

	for line := range strings.Lines(string(out)) {
		path, file, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && file != "" {
			exports[path] = file
		}
	}

	fset := token.NewFileSet()
	files := packageFilesIn(t, fset)

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

	checkedOnce = &checkedPackage{files: files, types: checked, info: info}

	return checkedOnce
}

// proofOf names the proof typ is, through aliases and one pointer.
func proofOf(typ types.Type) (string, bool) {
	typ = types.Unalias(typ)
	if p, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(p.Elem())
	}

	named, ok := typ.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != appPath {
		return "", false
	}

	_, proof := proofMakers[named.Obj().Name()]

	return named.Obj().Name(), proof
}

// carriedProofs are the proofs typ is or holds: through a pointer, slice,
// array, map or channel, a struct's fields, or a type of this package declared
// over any of those. A proof is a leaf: what it holds is its own business.
func carriedProofs(typ types.Type) []string {
	var out []string

	seen := map[types.Type]bool{}

	var walk func(types.Type)

	walk = func(typ types.Type) {
		typ = types.Unalias(typ)
		if seen[typ] {
			return
		}

		seen[typ] = true

		if name, proof := proofOf(typ); proof {
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

// ONLY THE STEP THAT PROVES A THING MAKES ITS PROOF.
//
// The control plane's order is its types: a Controller comes only from
// BecomeController, the node wire is served only with the two proofs only this
// controller's ForgetFleet and AdoptAuthority make, and scheduling needs the
// proof PublishAuthority makes. That holds while nothing else in this package
// creates one, which no compiler checks. So this type-checks every production
// file, package-level initializers included, and holds every way to make one to
// its maker: a literal of a proof type (however its type is spelled, or elided),
// new(T), a field of a proof written or addressed, a function returning one in
// any container or struct, and a conversion to one anywhere.
func TestOnlyTheProvingStepsMakeTheirProofs(t *testing.T) {
	t.Parallel()

	pkg := checkedApp(t)

	var violations []string

	report := func(where, what string) { violations = append(violations, where+" "+what) }

	// Every field of every proof, as the objects a selector resolves to, so a
	// field reached through an embedding or a chain (c.claim.Epoch) is still one.
	fieldOf := map[types.Object]string{}

	for name := range proofMakers {
		st, ok := pkg.types.Scope().Lookup(name).Type().Underlying().(*types.Struct)
		if !ok {
			t.Fatalf("the proof %s is not a struct", name)
		}

		for i := range st.NumFields() {
			fieldOf[st.Field(i)] = name
		}
	}

	// writtenProofs are the proofs whose fields an assigned or addressed
	// expression reaches, at any depth of its selector chain.
	writtenProofs := func(expr ast.Expr) []string {
		var out []string

		for expr != nil {
			switch x := expr.(type) {
			case *ast.SelectorExpr:
				if name, proof := fieldOf[pkg.info.Uses[x.Sel]]; proof {
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
					for _, name := range carriedProofs(results.At(i).Type()) {
						if proofMakers[name].maker != where {
							report(where, "returns a "+name)
						}
					}
				}
			}

			mayMake := func(name string) bool { return proofMakers[name].maker == where }

			var written []ast.Expr

			ast.Inspect(decl, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if name, proof := proofOf(pkg.info.TypeOf(x)); proof && !mayMake(name) {
						report(where, "builds a "+name)
					}
				case *ast.CallExpr:
					if tv, ok := pkg.info.Types[x.Fun]; ok && tv.IsType() {
						if name, proof := proofOf(tv.Type); proof {
							report(where, "converts to "+name)
						}
					}

					if id, ok := x.Fun.(*ast.Ident); ok && len(x.Args) == 1 {
						if b, isBuiltin := pkg.info.Uses[id].(*types.Builtin); isBuiltin && b.Name() == "new" {
							if name, proof := proofOf(pkg.info.TypeOf(x.Args[0])); proof && !mayMake(name) {
								report(where, "allocates a "+name)
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
						for _, name := range writtenProofs(x.X) {
							if !mayMake(name) {
								report(where, "takes the address of a field of a "+name)
							}
						}
					}
				}

				for _, lhs := range written {
					for _, name := range writtenProofs(lhs) {
						if !mayMake(name) {
							report(where, "sets a field of a "+name)
						}
					}
				}

				written = written[:0]

				return true
			})
		}
	}

	if len(violations) > 0 {
		t.Errorf("a proof is made outside the step that proves it, which hands it out "+
			"without the step: %v", violations)
	}
}

// EACH MAKER DOES ITS STEP BEFORE IT MAKES ITS PROOF. Only one function may make
// each proof; that is worth nothing if the function makes it without the step.
// So each maker must call its operation unconditionally, in a statement of its
// own body (not under a branch or a loop, in a goroutine, a deferred call or a
// closure), test that call's error in an `if` of its own body whose branch
// returns an error and builds no proof, and build its proof only after both. Two
// operations return no error to test: server.New, and the publication, which is
// non-fatal by design.
func TestEachMakerDoesItsStepBeforeItsProof(t *testing.T) {
	t.Parallel()

	for typ, m := range proofMakers {
		fn := findFunc(t, m.maker)

		var (
			calledAt, checkedAt, builtAt token.Pos
			branchProblem                string
			stack                        []ast.Node
		)

		// unconditional reports whether the call being visited runs whenever the
		// maker does: stack holds its ancestors, the body first.
		unconditional := func(call *ast.CallExpr) bool {
			for _, n := range stack {
				switch n.(type) {
				case *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
					return false
				}
			}

			if len(stack) < 2 {
				return false
			}

			switch top := stack[1].(type) {
			case *ast.AssignStmt, *ast.ExprStmt, *ast.DeclStmt:
				return true
			case *ast.IfStmt:
				return top.Init != nil && containsNode(top.Init, call)
			default:
				return false
			}
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]

				return false
			}

			switch x := n.(type) {
			case *ast.CallExpr:
				if calls(x, m.operation) && !calledAt.IsValid() && unconditional(x) {
					calledAt = x.Pos()
				}
			case *ast.IfStmt:
				holds := x.Init != nil && containsCall(x.Init, m.operation)
				after := calledAt.IsValid() && x.Pos() >= calledAt

				if (holds || after) && len(stack) == 1 && !checkedAt.IsValid() && testsErr(x.Cond) {
					checkedAt = x.Pos()
					branchProblem = refusesWithoutAProof(x.Body, typ)
				}
			case *ast.CompositeLit:
				if name, proof := proofName(x.Type); proof && name == typ && len(x.Elts) > 0 && !builtAt.IsValid() {
					builtAt = x.Pos()
				}
			}

			stack = append(stack, n)

			return true
		})

		errorless := m.operation == "server.New" || m.operation == "PublishSharedAuthority"

		switch {
		case !calledAt.IsValid():
			t.Errorf("%s no longer calls %s itself, so the %s it makes proves nothing", m.maker, m.operation, typ)
		case !builtAt.IsValid():
			t.Errorf("%s no longer builds the %s it proves", m.maker, typ)
		case builtAt < calledAt:
			t.Errorf("%s builds its %s before it calls %s", m.maker, typ, m.operation)
		case !errorless && (!checkedAt.IsValid() || checkedAt > builtAt):
			t.Errorf("%s builds its %s without testing %s's error first", m.maker, typ, m.operation)
		case !errorless && branchProblem != "":
			t.Errorf("%s's branch for a failed %s %s", m.maker, m.operation, branchProblem)
		}
	}
}

// refusesWithoutAProof says what is wrong with an error branch, or "" when it
// ends in a return whose last value is not nil (the error) and builds no proof
// of typ with its field set.
func refusesWithoutAProof(body *ast.BlockStmt, typ string) string {
	if len(body.List) == 0 {
		return "is empty"
	}

	ret, ok := body.List[len(body.List)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return "does not end in a return"
	}

	if id, ok := ret.Results[len(ret.Results)-1].(*ast.Ident); ok && id.Name == "nil" {
		return "returns no error"
	}

	problem := ""

	ast.Inspect(body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok {
			if name, proof := proofName(lit.Type); proof && name == typ && len(lit.Elts) > 0 {
				problem = "builds the proof anyway"
			}
		}

		return true
	})

	return problem
}

// proofName is the proof a type expression spells, through a pointer, for
// the makers' own literals, which spell their types.
func proofName(expr ast.Expr) (string, bool) {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}

	if id, ok := expr.(*ast.Ident); ok {
		_, proof := proofMakers[id.Name]

		return id.Name, proof
	}

	return "", false
}

// calls reports whether call invokes operation: "pkg.Name" a qualified call
// exactly, a bare name any call of that name.
func calls(call *ast.CallExpr, operation string) bool {
	pkg, name, qualified := strings.Cut(operation, ".")
	if !qualified {
		return calleeName(call) == operation
	}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}

	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == pkg
}

// containsNode reports whether n holds target.
func containsNode(n, target ast.Node) bool {
	found := false

	ast.Inspect(n, func(n ast.Node) bool {
		if n == target {
			found = true
		}

		return !found
	})

	return found
}

// containsCall reports whether n holds a call of operation.
func containsCall(n ast.Node, operation string) bool {
	found := false

	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calls(call, operation) {
			found = true
		}

		return true
	})

	return found
}

// testsErr reports whether cond is `err != nil` (or contains it).
func testsErr(cond ast.Expr) bool {
	found := false

	ast.Inspect(cond, func(n ast.Node) bool {
		if bin, ok := n.(*ast.BinaryExpr); ok && bin.Op == token.NEQ {
			if id, ok := bin.X.(*ast.Ident); ok && id.Name == "err" {
				found = true
			}
		}

		return true
	})

	return found
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

	ctl := &Controller{cp: &ControlPlane{cfg: cfg, host: Host{
		AuthorityLock: func(context.Context, string) (func() error, error) {
			locks++

			return nil, refused
		},
	}}}
	ctl.claim.Epoch = 1

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

// packageFilesIn parses this package's non-test sources into fset.
func packageFilesIn(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

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

// packageFiles parses this package's non-test sources.
func packageFiles(t *testing.T) []*ast.File {
	t.Helper()

	return packageFilesIn(t, token.NewFileSet())
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
