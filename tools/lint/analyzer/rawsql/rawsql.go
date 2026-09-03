// Package rawsql reports a call that executes SQL text billet did not name in a
// query file.
//
// WHAT THE RULE IS. All of billet's ledger SQL lives in
// internal/state/queries/*.sql, is compiled by sqlc into internal/state/sqlitedb,
// and is bound by internal/state/queryset.go. That is what puts every statement
// under the guards on that directory: each one is prepared against a real
// migrated ledger (the only thing that catches an ON CONFLICT whose unique index
// a migration removed, because sqlc models no indexes), classified read-or-write
// from its own first keyword, checked for a wildcard projection and checked for a
// non-ASCII byte that would corrupt sqlc's parameter rewriting. A statement
// written in Go is under none of it.
//
// WHY AN ANALYZER RATHER THAN A GREP. The only door to the ledger is
// database/sql, so watching that door is sound -- but the method names on it are
// ordinary English. `url.URL.Query`, `url.Values`, and any number of other types
// have a Query method, and this repository calls several of them. Deciding from a
// name alone means either false positives, which get a gate deleted, or a
// hand-maintained list of exceptions, which goes stale silently. Types settle it.
//
// HOW IT DECIDES. A call is SQL-executing when its callee is a method with one of
// database/sql's statement-executing names AND a signature that matches
// database/sql's: `(context.Context, string, ...any)` returning `(sql.Result,
// error)`, `(*sql.Rows, error)`, `*sql.Row` or `(*sql.Stmt, error)`, or the four
// non-context forms. Matching the SIGNATURE rather than the receiver is what
// makes a hand-rolled wrapper interface -- state.Querier, sqlc's DBTX, whatever
// somebody writes next -- covered rather than an escape hatch, and matching the
// RESULT types is what keeps every unrelated Query method quiet.
//
// # What it does not look at
//
//   - _test.go files. The same exemption depguard's ledgerwriters rule already
//     makes, for the same reason: the invariant is about what a transaction does
//     in a deployment, and a test binary is not that. internal/state's own tests
//     assert things about the ledger's schema that can only be asked directly.
//   - Generated files, as go/ast reports them. internal/state/sqlitedb IS the
//     compiled query set; its statements are the .sql files.
//
// There is no package scope list and no flag, deliberately. The ban is
// default-deny across the module, so a new package that reaches the ledger is
// refused rather than silently admitted -- and the conversions that made that
// possible landed before this did, so it arrives with zero unexplained
// violations rather than a backlog.
//
// # What can still get past it, and why that is the right line
//
// This is a gate against ACCIDENT, not against a determined bypass, and saying so
// is better than implying otherwise. Three things would escape:
//
//   - A file that adds `// Code generated ... DO NOT EDIT.` to itself. Nothing
//     distinguishes that from generated code, and refusing to trust the marker
//     would mean reporting internal/state/sqlitedb's ~180 calls.
//   - reflect, or a DEPENDENCY that takes the statement and reaches database/sql
//     inside itself under a name of its own -- an sqlx-style `Get(db, query, …)`.
//     Only this module is analysed, and the names checked are database/sql's own,
//     so such a helper is invisible here. depguard does NOT cover that: its global
//     rule is a lax DENYLIST, so a new dependency is admitted until somebody names
//     it. What stands there is review of go.mod, not a gate, and saying otherwise
//     would be worse than saying nothing.
//   - sql.Conn.Raw, which hands the caller a driver connection whose own Exec and
//     Query take a []driver.Value rather than a statement. Nothing in billet uses
//     it, and matching driver's shapes as well would report the generated package
//     and every driver implementation on the module path.
//   - A wrapper method whose results are billet's own types rather than
//     database/sql's. It is not reported, and it does not need to be: whatever it
//     is, it must reach database/sql somewhere IN THIS MODULE, and THAT call is.
//
// That last point is also why *sql.Stmt's Exec and Query are not a gap. They take
// arguments and no statement, so they do not match -- but a *sql.Stmt can only
// come from Prepare or PrepareContext, which do.
package rawsql

import (
	"go/ast"
	"go/types"

	"golang.org/x/tools/go/analysis"

	"github.com/junioryono/billet/tools/lint/suppress"
)

const analyzerName = "rawsql"

// Analyzer is the rawsql analyzer.
var Analyzer = &analysis.Analyzer{
	Name: analyzerName,
	Doc:  "report SQL executed from Go rather than named in a query file",
	Run:  run,
}

// executors are database/sql's statement-executing method names.
//
// THE NAME IS ONLY THE FIRST HALF of the test; the signature below is the other.
// A method called Query that does not take a statement and return database/sql's
// own row type is not this.
var executors = map[string]bool{
	"Exec":            true,
	"ExecContext":     true,
	"Query":           true,
	"QueryContext":    true,
	"QueryRow":        true,
	"QueryRowContext": true,
	"Prepare":         true,
	"PrepareContext":  true,
}

// sqlResults are the return shapes database/sql uses for those methods, written
// as the type string go/types produces.
var sqlResults = map[string]bool{
	"database/sql.Result":  true,
	"*database/sql.Rows":   true,
	"*database/sql.Row":    true,
	"*database/sql.Stmt":   true,
	"*database/sql.Result": true,
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		name := pass.Fset.Position(file.Pos()).Filename

		// GENERATED CODE IS THE COMPILED QUERY SET, not a hand-written statement,
		// and test files are exempt for the reason in the package comment.
		if ast.IsGenerated(file) || isTestFile(name) {
			continue
		}

		// ONLY READ A FILE THAT HAS SOMETHING TO INDEX. Pass.ReadFile refuses a
		// path outside the package it was handed, and the bytes are needed only to
		// tell a directive alone on its line from one trailing a statement.
		var ignores suppress.Lines

		if suppress.Present(file) {
			src, err := pass.ReadFile(name)
			if err != nil {
				return nil, err
			}

			ignores = suppress.Index(pass, file, src)
		}

		// EVERY IDENTIFIER, NOT EVERY CALL. Inspecting calls alone leaves two shapes
		// open, and neither is a call on a selector:
		//
		//   f := db.ExecContext         a method VALUE. The assignment is not a
		//                               call at all, and the later `f(ctx, stmt)`
		//                               has an identifier for its Fun.
		//   (*sql.DB).ExecContext       a method EXPRESSION, likewise.
		//
		// An identifier is what those and the ordinary call have in common, because
		// go/ast walks a selector's Sel as one. Nothing in billet takes a method
		// value, so the wider net costs nothing and closes the dodge.
		ast.Inspect(file, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if !ok || !executors[ident.Name] {
				return true
			}

			if !executesSQL(pass, ident) {
				return true
			}

			if ignores.Skip(pass, ident.Pos(), analyzerName) {
				return true
			}

			pass.Reportf(ident.Pos(),
				"%s executes SQL from Go. billet's ledger SQL is named in "+
					"internal/state/queries/*.sql and reached through state.ReadQueries or "+
					"state.WriteQueries, which is what puts it under `sqlc diff`, the "+
					"prepare-against-the-migrated-schema check and the read/write "+
					"classification. If this statement genuinely cannot be generated -- a "+
					"pragma, a statement naming sqlite_master, one whose TABLE is a "+
					"variable -- write `//billet:ignore %s // <why>` and add it to the "+
					"allowlist table in internal/state/queries/README.md",
				ident.Name, analyzerName)

			return true
		})

		ignores.ReportUnused(pass, analyzerName)
	}

	return nil, nil
}

// executesSQL reports whether this identifier USES a function with
// database/sql's shape.
//
// Uses RATHER THAN ObjectOf, because ObjectOf also resolves a DEFINING
// identifier -- the name in `func (readOnlyDBTX) ExecContext(...)`, and every
// method name inside an interface declaration. Those are not calls and reporting
// them would make the gate refuse the very adapters that exist to satisfy it.
//
// A RECEIVER IS REQUIRED, AND DROPPING IT WAS TRIED AND REVERTED. The idea was to
// catch a package-level FUNCTION with database/sql's shape, on the reasoning that
// it is the same door. It buys almost nothing and costs the property that matters
// most: a LOCAL such function must reach database/sql in its own body, which is
// already reported, so the only case it uniquely catches is an imported function
// named exactly one of these eight with exactly this signature -- while a
// legitimate stand-in, adapter or unsupported-operation stub with that shape would
// be refused for executing nothing. A gate that refuses correct code is one
// somebody deletes, and that trade is the wrong way round.
func executesSQL(pass *analysis.Pass, ident *ast.Ident) bool {
	fn, ok := pass.TypesInfo.Uses[ident].(*types.Func)
	if !ok {
		return false
	}

	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}

	return takesAStatement(sig) && returnsASQLType(sig)
}

// takesAStatement reports whether the parameters are database/sql's: an optional
// leading context, then the statement, then optionally variadic arguments.
func takesAStatement(sig *types.Signature) bool {
	params := sig.Params()

	at := 0
	if params.Len() > 0 && isContext(params.At(0).Type()) {
		at = 1
	}

	if params.Len() <= at {
		return false
	}

	basic, ok := params.At(at).Type().(*types.Basic)
	if !ok || basic.Kind() != types.String {
		return false
	}

	// Either the statement is the last parameter (Prepare) or the rest is the
	// variadic argument list.
	return params.Len() == at+1 || (params.Len() == at+2 && sig.Variadic())
}

// returnsASQLType reports whether any result is one of database/sql's own.
//
// ANY RATHER THAN ALL, because the four shapes differ: QueryRowContext returns
// *sql.Row alone while the others pair their type with an error. What they share
// is that database/sql's type appears, which is the property that distinguishes
// executing a statement from every other method called Query.
func returnsASQLType(sig *types.Signature) bool {
	results := sig.Results()
	for i := range results.Len() {
		if sqlResults[results.At(i).Type().String()] {
			return true
		}
	}

	return false
}

func isContext(t types.Type) bool {
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}

	obj := named.Obj()

	return obj != nil && obj.Name() == "Context" &&
		obj.Pkg() != nil && obj.Pkg().Path() == "context"
}

// isTestFile reports whether a path is a Go test file.
//
// BY SUFFIX, because analysis.Pass has no flag for it: a test package's Files
// include both the package's own sources and its _test.go ones.
func isTestFile(path string) bool {
	const suffix = "_test.go"

	return len(path) > len(suffix) && path[len(path)-len(suffix):] == suffix
}
