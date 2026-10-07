package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state/ledgertest"
)

// proofMakers names, for each value the control plane's order is built from,
// the one function allowed to create it, the fields only that function may set,
// and the operation it must have performed, successfully, before it does.
var proofMakers = map[string]struct {
	maker     string
	fields    []string
	operation string
}{
	"Controller":         {"BecomeController", []string{"cp", "claim", "loops"}, "becomeController"},
	"FleetForgotten":     {"ForgetFleet", []string{"forgottenBy"}, "ForgetEveryNode"},
	"AdoptedAuthority":   {"AdoptAuthority", []string{"adoptedBy"}, "AdoptSharedAuthority"},
	"AuthorityPublished": {"PublishAuthority", []string{"publishedBy"}, "PublishSharedAuthority"},
	"ServingWire":        {"ServeWire", []string{"servedBy"}, "ServeNodeWire"},
	"Scheduler":          {"Schedule", []string{"plane"}, "server.New"},
}

// proofFields are the fields whose names belong to a proof alone, so a keyed
// element naming one builds that proof whatever the literal's spelled type.
var proofFields = map[string]string{
	"forgottenBy": "FleetForgotten",
	"adoptedBy":   "AdoptedAuthority",
	"publishedBy": "AuthorityPublished",
	"servedBy":    "ServingWire",
}

// ONLY THE STEP THAT PROVES A THING MAKES ITS PROOF.
//
// The control plane's order is its types: a Controller comes only from
// BecomeController, the node wire is served only with the two proofs only this
// controller's ForgetFleet and AdoptAuthority make, and scheduling needs the
// proof PublishAuthority makes. That holds while nothing else in this package
// creates one, which no compiler checks, so this reads every production file
// whole, package-level initializers included, for every way to make one: a
// literal (its type spelled, or elided inside a literal of proofs), a keyed
// proof field, new(T), a conversion, a field written, a type declared on a
// proof (an alias builds it under another name), or a function returning one.
func TestOnlyTheProvingStepsMakeTheirProofs(t *testing.T) {
	t.Parallel()

	guardedField := map[string]string{}

	for typ, m := range proofMakers {
		for _, f := range m.fields {
			guardedField[f] = typ
		}
	}

	var violations []string

	report := func(where, what string) { violations = append(violations, where+" "+what) }

	for _, file := range packageFiles(t) {
		for _, decl := range file.Decls {
			where, body := "package scope", ast.Node(decl)

			if fn, ok := decl.(*ast.FuncDecl); ok {
				where = fn.Name.Name

				for _, typ := range returnedProofs(fn) {
					if proofMakers[typ].maker != where {
						report(where, "returns a "+typ)
					}
				}
			}

			mayMake := func(typ string) bool { return proofMakers[typ].maker == where }

			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok {
						if name, proof := proofName(ts.Type); proof {
							report(where, "declares "+ts.Name.Name+" on "+name)
						}
					}
				}
			}

			ast.Inspect(body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					if name, proof := proofName(x.Type); proof && !mayMake(name) {
						report(where, "builds a "+name+" literal")
					}

					// AN ELIDED ELEMENT TYPE IS THE CONTAINER'S: []AdoptedAuthority{{...}}
					// builds the proof with no type spelled at the inner literal.
					if elem, proof := elementProof(x.Type); proof && !mayMake(elem) {
						for _, elt := range x.Elts {
							if kv, ok := elt.(*ast.KeyValueExpr); ok {
								elt = kv.Value
							}

							if lit, ok := elt.(*ast.CompositeLit); ok && lit.Type == nil {
								report(where, "builds an elided "+elem)
							}
						}
					}
				case *ast.KeyValueExpr:
					if key, ok := x.Key.(*ast.Ident); ok {
						if typ, proof := proofFields[key.Name]; proof && !mayMake(typ) {
							report(where, "sets "+typ+"."+key.Name+" in a literal")
						}
					}
				case *ast.CallExpr:
					if name, proof := proofName(x.Fun); proof {
						report(where, "converts to "+name)
					}

					if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 {
						if name, proof := proofName(x.Args[0]); proof && !mayMake(name) {
							report(where, "allocates a "+name)
						}
					}
				case *ast.AssignStmt:
					for _, lhs := range x.Lhs {
						if sel, ok := lhs.(*ast.SelectorExpr); ok {
							if typ, guarded := guardedField[sel.Sel.Name]; guarded && !mayMake(typ) {
								report(where, "sets "+typ+"."+sel.Sel.Name)
							}
						}
					}
				}

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
// So each maker must call its operation, test the error that call returns, and
// build the proof with its field set only after both: a maker that dropped the
// call, ignored its error, or built the proof first fails here.
func TestEachMakerDoesItsStepBeforeItsProof(t *testing.T) {
	t.Parallel()

	for typ, m := range proofMakers {
		fn := findFunc(t, m.maker)

		var calledAt, checkedAt, builtAt token.Pos

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if calls(x, m.operation) && !calledAt.IsValid() {
					calledAt = x.Pos()
				}
			case *ast.IfStmt:
				// `if err := op(); err != nil` tests the call it holds; any other
				// `if err != nil` after the call tests what the call returned.
				holds := x.Init != nil && containsCall(x.Init, m.operation)
				after := calledAt.IsValid() && x.Pos() >= calledAt

				if (holds || after) && !checkedAt.IsValid() && testsErr(x.Cond) {
					checkedAt = x.Pos()
				}
			case *ast.CompositeLit:
				if name, proof := proofName(x.Type); proof && name == typ && len(x.Elts) > 0 && !builtAt.IsValid() {
					builtAt = x.Pos()
				}
			}

			return true
		})

		switch {
		case !calledAt.IsValid():
			t.Errorf("%s no longer calls %s, so the %s it makes proves nothing", m.maker, m.operation, typ)
		case !builtAt.IsValid():
			t.Errorf("%s no longer builds the %s it proves", m.maker, typ)
		case builtAt < calledAt && !containsCallBefore(fn, m.operation, builtAt):
			t.Errorf("%s builds its %s before it calls %s", m.maker, typ, m.operation)
		case m.operation != "server.New" && m.operation != "PublishSharedAuthority" &&
			(!checkedAt.IsValid() || checkedAt > builtAt):
			// server.New returns no error, and the publication is non-fatal by
			// design; every other operation's failure must stop the proof.
			t.Errorf("%s builds its %s without testing %s's error first", m.maker, typ, m.operation)
		}
	}
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

// containsCallBefore reports whether fn calls operation before pos.
func containsCallBefore(fn *ast.FuncDecl, operation string, pos token.Pos) bool {
	found := false

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calls(call, operation) && call.Pos() < pos {
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

// proofName is the proof a type expression names, through a pointer.
func proofName(expr ast.Expr) (string, bool) {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}

	if paren, ok := expr.(*ast.ParenExpr); ok {
		expr = paren.X
	}

	if id, ok := expr.(*ast.Ident); ok {
		_, proof := proofMakers[id.Name]

		return id.Name, proof
	}

	return "", false
}

// elementProof is the proof a container literal's elided elements build.
func elementProof(expr ast.Expr) (string, bool) {
	switch x := expr.(type) {
	case *ast.ArrayType:
		return proofName(x.Elt)
	case *ast.MapType:
		return proofName(x.Value)
	default:
		return "", false
	}
}

// returnedProofs are the proofs fn's results name, through pointers and
// containers.
func returnedProofs(fn *ast.FuncDecl) []string {
	if fn.Type.Results == nil {
		return nil
	}

	var out []string

	for _, field := range fn.Type.Results.List {
		ast.Inspect(field.Type, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if _, proof := proofMakers[id.Name]; proof {
					out = append(out, id.Name)
				}
			}

			return true
		})
	}

	return out
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
