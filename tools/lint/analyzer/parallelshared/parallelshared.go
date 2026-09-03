// Package parallelshared reports state a parallel subtest writes and its parent
// test owns.
//
// WHY THIS AND NOT paralleltest. billet disables paralleltest deliberately, and
// the reason is written down in .golangci.yml: what decides whether a test is
// safe is not whether it calls t.Parallel(), it is what the test SHARES with its
// neighbours. paralleltest and tparallel both reason about where t.Parallel() is
// placed and neither models what a parallel closure MUTATES, which is the actual
// hazard -- and the hazard is not a failed assertion. Two parallel subtests
// writing one map is `fatal error: concurrent map writes`, which ABORTS the test
// binary and reports every unrelated test in the package as failed, so the
// symptom points anywhere but the cause.
//
// WHAT MAKES IT WORTH AN ANALYZER RATHER THAN A REVIEW RULE: nothing at the call
// site says "parallel". A subtest can inherit parallelism from a harness that
// calls t.Parallel() itself, so reading the subtest body tells you nothing.
// Parallelism is detected through an exported analysis Fact, so a helper added
// later is picked up with no change here.
//
// # What it will not tell you
//
// Every limit below is a deliberate refusal to guess. A gate with false
// positives is a gate somebody deletes, so where the answer is unknowable the
// analyzer stays quiet -- and says so here rather than leaving it to be
// discovered.
//
//   - Package-level state. Sharing it under a mutex is legitimate, and lock
//     discipline is not statically decidable.
//   - Anything declared inside a for or range statement. Go 1.22 gives each
//     iteration its own copy, so the table-driven pattern shares nothing.
//   - Writes made through a pointer handed to another function.
//   - Reads. Two parallel subtests reading one map is fine.
//   - A function literal that is only DECLARED. Whether it runs is decided by a
//     call, and the call is what gets followed. A deferred literal, an
//     immediately-invoked one, one handed to t.Cleanup and one started with `go`
//     all DO run inside the subtest, so all four are covered.
//   - A closure reached other than lexically: through a struct field
//     (`holder.fn()`), returned and immediately invoked (`factory()()`), a method
//     value, or interface dispatch. Each is a call that provably runs something
//     this cannot resolve, and closing them in general is how a lexical checker
//     turns into a call-graph one.
//   - A closure handed to a function that is not a known callback-runner.
//     `register(func(){…})` may call it, store it or drop it.
//   - WHICH PARAMETER a parallel helper makes parallel. The Fact is a boolean, so
//     a helper taking two *testing.T is not modelled, and neither is one called
//     with a captured outer t. Measured on this repository: zero helpers make a
//     T parallel and zero functions take more than one, so the parameter-index
//     machinery is not written on speculation. If either shape appears, this is
//     the note to come back to.
package parallelshared

import (
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"

	"github.com/junioryono/billet/tools/lint/suppress"
)

const analyzerName = "parallelshared"

// Analyzer is the parallelshared analyzer.
var Analyzer = &analysis.Analyzer{
	Name:      analyzerName,
	Doc:       "report state a parallel subtest writes and its parent test owns",
	Run:       run,
	FactTypes: []analysis.Fact{(*parallelHelper)(nil)},
}

// parallelHelper marks a function that makes a *testing.T parallel.
//
// AN EXPORTED FACT rather than a name check, because the helper that does this
// in a given package is not knowable from here and moves: `newHarness(t)` and
// `newTestServer(t)` are the same hazard under two names, and a list of names
// goes stale silently -- which for a gate means it stops detecting and still
// reports zero, indistinguishable from a clean tree.
type parallelHelper struct{}

func (*parallelHelper) AFact() {}

func (*parallelHelper) String() string { return "parallelHelper" }

// checker carries what every step needs: the pass, the file's suppressions, the
// assignment index, and what has already been reported.
type checker struct {
	pass    *analysis.Pass
	ignores suppress.Lines
	// assigned indexes every assignment in the package ONCE. Resolving a helper
	// closure and resolving a *testing.T alias both ask about a variable while
	// walking, and re-scanning every file per question is quadratic in a package
	// with a large test file.
	assigned map[*types.Var]*assignment
	// reported deduplicates write sites across one top-level test.
	reported map[token.Pos]bool
}

func run(pass *analysis.Pass) (any, error) {
	assigned := indexAssignments(pass)

	markParallelHelpers(pass, assigned)

	for _, file := range pass.Files {
		// ONLY READ A FILE THAT HAS SOMETHING TO INDEX. The bytes are needed to
		// tell a directive alone on its line from one trailing a statement, but
		// asking for them unconditionally failed the whole run on Linux CI: this
		// analyzer carries a Fact, so it runs over every dependency, and
		// Pass.ReadFile refuses a path outside the package it was handed --
		// runtime/cgo's source is not among that package's own files.
		var ignores suppress.Lines

		if suppress.Present(file) {
			src, err := pass.ReadFile(pass.Fset.Position(file.Pos()).Filename)
			if err != nil {
				return nil, err
			}

			ignores = suppress.Index(pass, file, src)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !isTestFunction(pass, fn) {
				continue
			}

			c := &checker{
				pass:     pass,
				ignores:  ignores,
				assigned: assigned,
				reported: map[token.Pos]bool{},
			}

			c.checkBody(scope{
				body: fn.Body,
				ownT: testingTVar(pass, fn.Type),
			}, map[*ast.FuncLit]bool{})
		}

		// AFTER the file's tests, because a directive is only unused once
		// everything that could have consumed it has run.
		ignores.ReportUnused(pass, analyzerName)
	}

	return nil, nil
}

// scope is one test or t.Run group: its body, the *testing.T that body owns, and
// what the enclosing scopes declared.
//
// ownT IS AN IDENTITY, NOT A TYPE. Recognising t.Run, t.Cleanup and t.Parallel by
// the TYPE of their receiver treats a captured OUTER t as if it were the
// subtest's own, and the two mean different things:
//
//	t.Run("child", func(child *testing.T) {
//	    child.Parallel()
//	    t.Cleanup(func() { shared["x"] = 1 })
//	})
//
// that cleanup belongs to the outer test and runs after its subtests finish, so
// reporting it as the child's write is a false positive. A captured outer
// t.Parallel() would equally have marked the child parallel when it is not.
type scope struct {
	body  *ast.BlockStmt
	ownT  *types.Var
	owned []*types.Var
}

// checkBody walks one test (or t.Run group) body, collecting what it declares and
// checking every parallel subtest it starts.
//
// owned is what the ENCLOSING scopes declared. A nested group adds its own
// declarations, so a subtest three levels down is checked against everything
// above it.
func (c *checker) checkBody(sc scope, active map[*ast.FuncLit]bool) {
	owned := append(append([]*types.Var{}, sc.owned...), c.declaredDirectlyIn(sc.body)...)

	for _, run := range c.subtests(sc.body, sc.ownT) {
		runT := testingTVar(c.pass, run.Type)

		if c.isParallel(run, runT) {
			c.reportWrites(run, runT, owned)

			continue
		}

		// A STACK, NOT A VISITED SET. A callback that names itself --
		// `f = func(t *testing.T) { t.Run("again", f) }` -- would otherwise
		// re-enter its own body until the analyzer exhausts the stack, which is
		// the same crash the helper-closure recursion caused one function down.
		// Entries are removed on the way out, so the same callback can still be
		// analysed independently from a different legitimate owning scope.
		if active[run] {
			continue
		}

		active[run] = true

		// A SEQUENTIAL GROUP IS STILL A SCOPE. `t.Run("group", func(t *testing.T)
		// { … t.Run("case", parallel) … })` is the ordinary shape, and its own
		// declarations are shared by every parallel case inside it.
		c.checkBody(scope{body: run.Body, ownT: runT, owned: owned}, active)

		delete(active, run)
	}
}

// reportWrites walks a parallel subtest and reports every write to owned state.
func (c *checker) reportWrites(fn *ast.FuncLit, ownT *types.Var, owned []*types.Var) {
	inner := c.declaredAnywhereIn(fn.Body)

	seen := map[*types.Var]bool{}

	// A CLOSURE IS ENTERED ONCE. A helper that calls itself -- `f = func() {
	// shared++; f() }` -- would otherwise re-enter its own body until the stack
	// runs out, which turns a lint gate into a crash. Bounding it changes nothing
	// about what is reported: a second visit could only find writes the first one
	// already found.
	entered := map[*ast.FuncLit]bool{fn: true}

	var walk func(n ast.Node) bool

	enter := func(lit *ast.FuncLit) {
		if lit == nil || entered[lit] {
			return
		}

		entered[lit] = true

		ast.Inspect(lit.Body, walk)
	}

	walk = func(n ast.Node) bool {
		// A FUNCTION LITERAL IS NEVER ENTERED BY FALLING INTO IT. Whether a
		// closure RUNS is decided by something reaching it, so the traversal stops
		// at every literal and `executed` opens the ones that do. Descending by
		// default and excluding what looked "stored" is the other way round, and
		// it reported a literal in a slice -- or one handed to a function that
		// only files it away -- as a write that may never happen.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}

		for _, target := range writeTargets(n) {
			c.report(target, owned, inner, seen)
		}

		if call, ok := n.(*ast.CallExpr); ok {
			for _, lit := range c.executed(call, ownT) {
				enter(lit)
			}
		}

		return true
	}

	ast.Inspect(fn.Body, walk)
}

// report emits one diagnostic for a write to state the enclosing test owns.
func (c *checker) report(
	target ast.Expr, owned []*types.Var, inner, seen map[*types.Var]bool,
) {
	v := c.rootVar(target)
	if v == nil || inner[v] || !containsVar(owned, v) || seen[v] {
		return
	}

	if c.ignores.Skip(c.pass, target.Pos(), analyzerName) {
		return
	}

	// ONE DIAGNOSTIC PER WRITE SITE, ACROSS THE WHOLE TEST. `seen` covers this
	// subtest; `reported` covers the same literal being reached twice, which one
	// callback used as two subtests does:
	//
	//	cb := func(t *testing.T) { t.Parallel(); shared["x"] = 1 }
	//	t.Run("one", cb)
	//	t.Run("two", cb)
	//
	// The cycle stack cannot help there -- a parallel callback is reported rather
	// than recursed into, so it never enters the stack -- and the second report
	// would name the same line twice.
	if c.reported[target.Pos()] {
		return
	}

	c.reported[target.Pos()] = true
	seen[v] = true

	c.pass.Reportf(target.Pos(),
		"%s is declared by the enclosing test and written here, in a subtest that "+
			"runs in parallel. Two subtests writing it race; a racing map is "+
			"`fatal error: concurrent map writes`, which aborts the whole test binary "+
			"rather than failing one test. Give each subtest its own, or make the "+
			"subtest sequential",
		v.Name())
}

// executed returns the closures a call causes to run.
//
// THE CALLEE, plus the callback of the testing methods that are DEFINED to run
// one. `defer func(){…}()` and `go func(){…}()` need no case of their own: both
// carry a CallExpr whose Fun is the literal, and ast.Inspect reaches it.
func (c *checker) executed(call *ast.CallExpr, ownT *types.Var) []*ast.FuncLit {
	out := []*ast.FuncLit{c.closureFor(call.Fun)}

	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !c.isOwnT(sel.X, ownT) {
		return out
	}

	switch sel.Sel.Name {
	case "Cleanup":
		if len(call.Args) == 1 {
			out = append(out, c.closureFor(call.Args[0]))
		}

	// A nested subtest of a parallel subtest runs inside it, so a write there
	// races the sibling exactly the same way.
	case "Run":
		if len(call.Args) == 2 {
			out = append(out, c.closureFor(call.Args[1]))
		}
	}

	return out
}

// writeTargets returns the expressions a statement writes to.
func writeTargets(n ast.Node) []ast.Expr {
	switch s := n.(type) {
	case *ast.AssignStmt:
		return s.Lhs

	case *ast.IncDecStmt:
		return []ast.Expr{s.X}

	case *ast.CallExpr:
		// delete and clear are writes the runtime performs on the caller's behalf,
		// and neither looks like an assignment.
		ident, ok := s.Fun.(*ast.Ident)
		if !ok || len(s.Args) == 0 {
			return nil
		}

		if ident.Name == "delete" || ident.Name == "clear" {
			return []ast.Expr{s.Args[0]}
		}

		return nil

	default:
		return nil
	}
}

// rootVar walks to the identifier an assignment target is rooted at.
//
// `m[k] = v` and `s.field = v` both write memory the enclosing test owns, so the
// question is what the expression is ROOTED at rather than whether the whole
// expression is a bare name.
func (c *checker) rootVar(expr ast.Expr) *types.Var {
	for {
		switch e := expr.(type) {
		case *ast.Ident:
			// Uses, never Defs: `x := …` inside the subtest DECLARES a new x, which
			// shadows rather than writes, and Defs is how the two are told apart.
			v, _ := c.pass.TypesInfo.Uses[e].(*types.Var)

			return v

		case *ast.IndexExpr:
			expr = e.X

		case *ast.SelectorExpr:
			expr = e.X

		case *ast.StarExpr:
			expr = e.X

		case *ast.ParenExpr:
			expr = e.X

		default:
			return nil
		}
	}
}

// closureFor resolves an expression to the function literal behind it.
//
// ONE RESOLVER FOR BOTH SEAMS -- a subtest callback and a helper call -- because
// `t.Run("x", caseFn)` and `record()` are the same question asked twice, and two
// implementations would answer it differently. A literal resolves to itself; a
// name resolves to the single literal assigned to it, whether that was
// `f := func(){}` or `var f = func(){}`.
func (c *checker) closureFor(expr ast.Expr) *ast.FuncLit {
	switch e := unparen(expr).(type) {
	case *ast.FuncLit:
		return e

	case *ast.Ident:
		v, _ := c.pass.TypesInfo.Uses[e].(*types.Var)
		if v == nil {
			return nil
		}

		lit, _ := unparen(c.soleValue(v)).(*ast.FuncLit)

		return lit

	default:
		return nil
	}
}

// assignment is what a variable was assigned, and how many times.
//
// value is nil when the sole assignment has no one-to-one right-hand side -- a
// multi-result call -- which still COUNTS, because the variable was reassigned,
// and resolves to nothing, which is the honest answer.
type assignment struct {
	count int
	value ast.Expr
}

// indexAssignments records every assignment in the package.
func indexAssignments(pass *analysis.Pass) map[*types.Var]*assignment {
	out := map[*types.Var]*assignment{}

	record := func(name *ast.Ident, value ast.Expr) {
		v := identVar(pass, name)
		if v == nil {
			return
		}

		a := out[v]
		if a == nil {
			a = &assignment{}
			out[v] = a
		}

		a.count++

		if value != nil {
			a.value = value
		}
	}

	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch st := n.(type) {
			case *ast.AssignStmt:
				for i, lhs := range st.Lhs {
					// UNPARENTHESISED FIRST: `(record) = func(){}` is legal Go, and
					// asserting the identifier directly skipped it -- leaving the
					// variable looking singly-assigned and resolving to a literal that
					// no longer runs.
					name, ok := unparen(lhs).(*ast.Ident)
					if !ok {
						continue
					}

					// A multi-result assignment has no expression of its own for this
					// name. It still counts -- the variable was reassigned -- and
					// resolves to nothing. Stopping at the arity mismatch instead left
					// a stale literal standing, so `_, f = pair()` kept resolving to
					// whatever f had been declared with.
					if len(st.Rhs) == len(st.Lhs) {
						record(name, st.Rhs[i])

						continue
					}

					record(name, nil)
				}

			// `var f func()` declares without assigning, so it is not recorded: the
			// ordinary declare-then-assign shape would otherwise resolve to nothing.
			case *ast.ValueSpec:
				for i, name := range st.Names {
					switch {
					case len(st.Values) == len(st.Names):
						record(name, st.Values[i])
					case len(st.Values) > 0:
						record(name, nil)
					}
				}
			}

			return true
		})
	}

	return out
}

// soleValue returns what v was assigned, when it was assigned exactly once.
//
// More than one assignment and the variable is ambiguous: what a later call
// reaches is not knowable from here, and following the first literal would
// report code that does not run.
func (c *checker) soleValue(v *types.Var) ast.Expr {
	a := c.assigned[v]
	if a == nil || a.count != 1 {
		return nil
	}

	return a.value
}

// subtests returns the closures this body passes to t.Run.
//
// IT DOES NOT DESCEND INTO A SUBTEST IT HAS ALREADY FOUND. Walking the whole
// tree would return a nested t.Run twice -- once as a child of this body and
// once as a child of the group above it -- and report the same write twice, from
// two different owned sets. Each level is handled by its own checkBody call, and
// that is what makes the owned set at each level mean something.
func (c *checker) subtests(body *ast.BlockStmt, ownT *types.Var) []*ast.FuncLit {
	var out []*ast.FuncLit

	var walk func(n ast.Node) bool

	walk = func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Run" || len(call.Args) != 2 || !c.isOwnT(sel.X, ownT) {
			return true
		}

		// RESOLVED RATHER THAN TYPE-ASSERTED: `t.Run("x", caseFn)` is the same
		// subtest as an inline literal, and accepting only the literal missed it.
		lit := c.closureFor(call.Args[1])
		if lit == nil {
			return true
		}

		out = append(out, lit)

		// ONLY THE NAME ARGUMENT. `t.Run(name(t.Run(...)), f)` is absurd but
		// legal, so the first argument is still walked; the callback is the
		// subtest's own body and belongs to its checkBody. Comparing the argument
		// against the resolved literal was not enough -- for `t.Run("x", (func…))`
		// the argument is a ParenExpr, so it did not compare equal, and the body
		// was discovered twice with two different owned sets.
		ast.Inspect(call.Args[0], walk)

		return false
	}

	for _, stmt := range body.List {
		ast.Inspect(stmt, walk)
	}

	return out
}

// declaredDirectlyIn returns the variables a body declares OUTSIDE any for or
// range statement.
//
// A DECLARATION INSIDE A LOOP IS PER-ITERATION, so two subtests started from two
// iterations never see the same variable -- which is the table-driven pattern,
// and including it would make this analyzer fire on almost every test in the
// tree.
func (c *checker) declaredDirectlyIn(body *ast.BlockStmt) []*types.Var {
	var out []*types.Var

	var walk func(n ast.Node) bool

	walk = func(n ast.Node) bool {
		switch n.(type) {
		case *ast.ForStmt, *ast.RangeStmt:
			return false

		case *ast.FuncLit:
			// A closure's own declarations belong to the closure.
			return false
		}

		for _, ident := range declaredIdents(n) {
			if v, ok := c.pass.TypesInfo.Defs[ident].(*types.Var); ok && v != nil {
				out = append(out, v)
			}
		}

		return true
	}

	for _, stmt := range body.List {
		ast.Inspect(stmt, walk)
	}

	return out
}

// declaredAnywhereIn returns every variable declared inside a body, closures and
// loops included: a subtest writing something it declared itself shares nothing.
func (c *checker) declaredAnywhereIn(body *ast.BlockStmt) map[*types.Var]bool {
	out := map[*types.Var]bool{}

	ast.Inspect(body, func(n ast.Node) bool {
		for _, ident := range declaredIdents(n) {
			if v, ok := c.pass.TypesInfo.Defs[ident].(*types.Var); ok && v != nil {
				out[v] = true
			}
		}

		return true
	})

	return out
}

// declaredIdents returns the identifiers a node declares.
func declaredIdents(n ast.Node) []*ast.Ident {
	switch s := n.(type) {
	case *ast.AssignStmt:
		if s.Tok.String() != ":=" {
			return nil
		}

		var out []*ast.Ident

		for _, lhs := range s.Lhs {
			if ident, ok := lhs.(*ast.Ident); ok {
				out = append(out, ident)
			}
		}

		return out

	case *ast.ValueSpec:
		return s.Names

	default:
		return nil
	}
}

// isParallel reports whether a subtest closure runs in parallel, directly or
// through a helper carrying the fact.
func (c *checker) isParallel(fn *ast.FuncLit, ownT *types.Var) bool {
	return makesParallel(c.pass, c.assigned, fn.Body, ownT)
}

// makesParallel reports whether a body makes the given *testing.T parallel.
//
// IT DOES NOT DESCEND INTO A NESTED CLOSURE, and that is the whole correctness
// of it. A function containing `t.Run("x", func(t *testing.T) { t.Parallel() })`
// makes its own t parallel not at all -- that Parallel call belongs to the
// SUBTEST's t, a different variable with the same name. Walking the whole tree
// marked every test function in the fixture as a parallel helper.
func makesParallel(
	pass *analysis.Pass, assigned map[*types.Var]*assignment,
	body *ast.BlockStmt, ownT *types.Var,
) bool {
	found := false

	var walk func(n ast.Node) bool

	walk = func(n ast.Node) bool {
		if found {
			return false
		}

		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}

		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Parallel" &&
			ownT != nil && originOf(pass, assigned, identVar(pass, sel.X)) == ownT {
			found = true

			return false
		}

		if callsParallelHelper(pass, call) {
			found = true

			return false
		}

		return true
	}

	for _, stmt := range body.List {
		ast.Inspect(stmt, walk)
	}

	return found
}

// callsParallelHelper reports whether a call reaches a function billet knows
// makes a *testing.T parallel.
func callsParallelHelper(pass *analysis.Pass, call *ast.CallExpr) bool {
	fn := calleeFunc(pass, call)
	if fn == nil {
		return false
	}

	var fact parallelHelper

	return pass.ImportObjectFact(fn, &fact)
}

// markParallelHelpers exports the fact for every function in this package whose
// body makes its own *testing.T parallel, to a fixpoint so a helper calling a
// helper is covered.
func markParallelHelpers(pass *analysis.Pass, assigned map[*types.Var]*assignment) {
	type candidate struct {
		obj  *types.Func
		body *ast.BlockStmt
		ownT *types.Var
	}

	var candidates []candidate

	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !takesTestingT(pass, fn.Type) {
				continue
			}

			// A TEST IS NOT A HELPER. Nothing calls TestXxx, so a fact on one can
			// never be read, and exporting it would publish a fact per test in
			// every package this runs over.
			if isTestFunction(pass, fn) {
				continue
			}

			obj, _ := pass.TypesInfo.Defs[fn.Name].(*types.Func)
			if obj == nil {
				continue
			}

			candidates = append(candidates, candidate{
				obj:  obj,
				body: fn.Body,
				ownT: testingTVar(pass, fn.Type),
			})
		}
	}

	for changed := true; changed; {
		changed = false

		for _, c := range candidates {
			var already parallelHelper
			if pass.ImportObjectFact(c.obj, &already) {
				continue
			}

			if !makesParallel(pass, assigned, c.body, c.ownT) {
				continue
			}

			pass.ExportObjectFact(c.obj, new(parallelHelper))

			changed = true
		}
	}
}

// calleeFunc resolves the function a call names, if it names one directly.
func calleeFunc(pass *analysis.Pass, call *ast.CallExpr) *types.Func {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		obj, _ := pass.TypesInfo.Uses[fun].(*types.Func)

		return obj

	case *ast.SelectorExpr:
		obj, _ := pass.TypesInfo.Uses[fun.Sel].(*types.Func)

		return obj

	default:
		return nil
	}
}

// isOwnT reports whether an expression is the *testing.T this scope owns.
//
// THE RESOLUTION IS ONE-WAY, and that asymmetry is the point. ownT is the FORMAL
// PARAMETER and is never normalised; only the receiver expression is followed
// down to see whether it lands there. Normalising both ends would let a rebound
// parameter change what a scope owns --
//
//	t.Run("child", func(child *testing.T) { child = outer; child.Parallel() })
//
// would classify the child as parallel although Parallel was called on the outer
// handle -- and it is what made a cycle's answer depend on which end you started
// from. A parameter has no assignment to follow, so the fixed end is stable by
// construction.
func (c *checker) isOwnT(expr ast.Expr, ownT *types.Var) bool {
	if ownT == nil {
		return false
	}

	return c.originOf(identVar(c.pass, expr)) == ownT
}

// originOf follows a *testing.T through single-assignment aliases to the
// variable it ultimately came from.
//
// AN ALIAS IS THE SAME HANDLE UNDER ANOTHER NAME, and comparing raw identities
// treated it as a different one -- so `alias := t; alias.Run(…)` found no
// subtests at all, and a file that consistently aliased its handles would be
// silently unanalysed while the fixture stayed green. `t := t` inside a loop is
// the same shape and does occur in this repository.
//
// Only a SOLE assignment is followed, for the reason soleValue gives, and a
// visited set bounds a cycle rather than trusting the type checker to have
// rejected one.
func (c *checker) originOf(v *types.Var) *types.Var {
	return originOf(c.pass, c.assigned, v)
}

func originOf(pass *analysis.Pass, assigned map[*types.Var]*assignment, v *types.Var) *types.Var {
	start := v
	seen := map[*types.Var]bool{}

	for v != nil {
		// A CYCLE ANSWERS WITH WHERE IT STARTED, not with whichever variable the
		// walk happened to revisit. `alias := child; child = alias` has no origin
		// -- both ends are equally derived -- and returning an arbitrary one made
		// the answer depend on which end the question came from, so the same call
		// resolved differently in two places. Returning start is deterministic,
		// and the consequence is a miss on pathological code rather than a
		// classification that contradicts itself.
		if seen[v] {
			return start
		}

		seen[v] = true

		a := assigned[v]
		if a == nil || a.count != 1 || a.value == nil {
			return v
		}

		next := identVar(pass, a.value)
		if next == nil || !isTestingTType(next.Type()) {
			return v
		}

		v = next
	}

	return v
}

// identVar returns the variable an expression names, if it names one.
func identVar(pass *analysis.Pass, expr ast.Expr) *types.Var {
	ident, ok := unparen(expr).(*ast.Ident)
	if !ok {
		return nil
	}

	if v, ok := pass.TypesInfo.Uses[ident].(*types.Var); ok {
		return v
	}

	v, _ := pass.TypesInfo.Defs[ident].(*types.Var)

	return v
}

// testingTVar returns the variable a signature's first *testing.T parameter
// declares, which is the T that function body owns.
func testingTVar(pass *analysis.Pass, ft *ast.FuncType) *types.Var {
	if ft.Params == nil {
		return nil
	}

	for _, field := range ft.Params.List {
		tv, ok := pass.TypesInfo.Types[field.Type]
		if !ok || !isTestingTType(tv.Type) || len(field.Names) == 0 {
			continue
		}

		if v, ok := pass.TypesInfo.Defs[field.Names[0]].(*types.Var); ok {
			return v
		}
	}

	return nil
}

func isTestingTType(t types.Type) bool {
	ptr, ok := t.(*types.Pointer)
	if !ok {
		return false
	}

	named, ok := ptr.Elem().(*types.Named)
	if !ok {
		return false
	}

	obj := named.Obj()

	return obj != nil && obj.Name() == "T" &&
		obj.Pkg() != nil && obj.Pkg().Path() == "testing"
}

// takesTestingT reports whether a signature accepts a *testing.T.
func takesTestingT(pass *analysis.Pass, ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}

	for _, field := range ft.Params.List {
		tv, ok := pass.TypesInfo.Types[field.Type]
		if ok && isTestingTType(tv.Type) {
			return true
		}
	}

	return false
}

// isTestFunction reports whether a declaration is a top-level Go test.
func isTestFunction(pass *analysis.Pass, fn *ast.FuncDecl) bool {
	return strings.HasPrefix(fn.Name.Name, "Test") && fn.Recv == nil &&
		takesTestingT(pass, fn.Type)
}

// unparen strips redundant parentheses from an expression.
func unparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}

		expr = paren.X
	}
}

func containsVar(vars []*types.Var, want *types.Var) bool {
	for _, v := range vars {
		if v == want {
			return true
		}
	}

	return false
}
