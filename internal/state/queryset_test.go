package state

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"modernc.org/sqlite"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

const (
	generatedDir = "ledgerdb"
	queriesDir   = "queries"
)

// generatedQuery is one compiled statement sqlc emitted as a Go constant.
type generatedQuery struct {
	Name string
	File string
	SQL  string
}

// nameAnnotation is the `-- name: X :one` line sqlc keeps at the head of every
// query constant. It is what distinguishes those constants from any other string
// constant in the package.
var nameAnnotation = regexp.MustCompile(`\A\s*--\s*name:\s*([A-Za-z_][A-Za-z0-9_]*)\s`)

// sourceAnnotation matches the same line in the .sql definitions.
var sourceAnnotation = regexp.MustCompile(`(?m)^--\s*name:\s*([A-Za-z_][A-Za-z0-9_]*)\s`)

// EVERY COMPILED QUERY MUST PREPARE AGAINST THE SCHEMA THE MIGRATIONS PRODUCE.
//
// WHAT THIS CATCHES THAT NOTHING ELSE DOES, AND IT IS NOW MORE THAN IT WAS.
// Generation runs once, with the PostgreSQL engine, so sqlc's catalogue comes
// from internal/state/pgmigrations: a query naming a column that is gone fails
// at `sqlc generate` against THAT schema. This is the only thing in the tree
// that asks the same question of SQLite's, which the two timelines being proved
// equivalent makes a check rather than a formality.
//
// And it still covers what sqlc does not model on either engine. It does not
// model INDEXES, and an upsert cannot be prepared without one:
//
//	INSERT ... ON CONFLICT (org, runner_group, label) DO UPDATE
//
// needs a UNIQUE index over those columns, and SQLite answers "ON CONFLICT
// clause does not match any PRIMARY KEY or UNIQUE constraint". sqlc generates
// that query happily, the Go compiles, the schema is valid and `sqlc diff` is
// clean — nothing fails until SQLite prepares that one statement, which happens
// on a real scheduling write. billet has three such upserts and four migrations
// that rebuild a table by DROP/RENAME, so the hazard is not hypothetical.
//
// IT ALSO PROVES THE PLACEHOLDERS RUN HERE. The generated constants carry $N,
// because that is what the PostgreSQL engine emits and a ?N constant would not
// execute on PostgreSQL at all. SQLite treats $1 as a named parameter and
// assigns indices by first appearance, which is exactly sqlc's numbering — so
// every statement preparing against a real SQLite ledger is what says the shared
// query set is not a PostgreSQL-only one that happens to compile.
//
// WHY PREPARE IS ENOUGH HERE, and it is worth saying because on other engines it
// is not: PostgreSQL's PREPARE defers planning to EXECUTE and accepts a broken
// arbiter. SQLite's does not. Measured against a real migrated ledger, prepare
// REFUSES a missing column, a missing table, a syntax error, a bad column inside
// an ON CONFLICT SET, and an ON CONFLICT with no matching unique index — so a
// statement that prepares has had every object it names resolved. Preparing also
// executes nothing, which is why this needs no transaction, no rollback and no
// discrimination between a schema error and a constraint violation.
func TestEveryGeneratedQueryPreparesAgainstTheMigratedSchema(t *testing.T) {
	db := open(t)

	for _, q := range loadGeneratedQueries(t) {
		requirePreparable(t, db, q)
	}
}

// requirePreparable is its own function so a failed prepare returns rather than
// falling through to a close, and the statement cannot outlive the check.
func requirePreparable(t *testing.T, db *DB, q generatedQuery) {
	t.Helper()

	stmt, err := db.w.PrepareContext(t.Context(), q.SQL)
	if err != nil {
		t.Errorf("%s (%s) cannot be prepared against the current schema: %v\n%s",
			q.Name, q.File, err, q.SQL)

		return
	}

	// sqlclosecheck asks for a defer and gocritic's unnecessaryDefer refuses one
	// placed immediately before the return, so the two cannot both be satisfied
	// here. A direct close is the correct code: nothing runs between the prepare
	// and this line, so there is no path on which a defer would save the handle.
	//nolint:sqlclosecheck // no statement between the prepare and this close.
	if err := stmt.Close(); err != nil {
		t.Errorf("close the prepared %s: %v", q.Name, err)
	}
}

// THE COMPLETENESS ORACLE FOR THE TEST ABOVE.
//
// A floor ("at least twenty queries loaded") cannot detect a loader that stopped
// matching one FILE — the count stays comfortably above it while a whole domain
// goes unchecked. Comparing the extracted constants against the `-- name:`
// annotations in the sources is exact, and it catches the other direction too: a
// generated file orphaned by a deleted query still compiles, and `sqlc diff`
// does not remove it.
func TestEveryGeneratedQueryIsNamedInASourceFile(t *testing.T) {
	generated := loadGeneratedQueries(t)

	var compiled []string
	for _, q := range generated {
		compiled = append(compiled, q.Name)
	}

	declared := sourceQueryNames(t)

	sort.Strings(compiled)
	sort.Strings(declared)

	if len(declared) == 0 {
		t.Fatal("no -- name: annotations found in " + queriesDir +
			"; this comparison would have checked nothing")
	}

	for _, name := range declared {
		if !slices.Contains(compiled, name) {
			t.Errorf("%s is declared in %s and has no generated constant; run `make sqlc`",
				name, queriesDir)
		}
	}

	for _, name := range compiled {
		if !slices.Contains(declared, name) {
			t.Errorf("%s has a generated constant in %s and is declared in no query file; "+
				"it is left over from a deleted query and `sqlc diff` cannot see it",
				name, generatedDir)
		}
	}
}

// ReadOps MUST CONTAIN EXACTLY THE QUERIES WHOSE SQL ONLY READS.
//
// THE ORACLE IS THE SQL, NOT THE GENERATED CODE, and that distinction is the
// whole value of this test. Asking "does this method reach ExecContext" and
// calling everything else a mutation is circular: it classifies by dispatch, and
// dispatch is chosen by the query's `:one`/`:many`/`:exec` annotation rather than
// by what the statement does. `UPDATE … RETURNING` declared `:one` is generated
// as QueryRowContext, which readOnlyDBTX forwards unconditionally — so such a
// query listed in ReadOps would mutate through a handle that must not, and a
// dispatch-based check would agree that everything was fine.
//
// So the classification is read from the first keyword of each statement in
// internal/state/queries, independently of anything sqlc decided.
func TestReadOpsHoldsExactlyTheQueriesThatOnlyRead(t *testing.T) {
	readable := methodNames(reflect.TypeOf((*ReadOps)(nil)).Elem())

	reads := readOnlyQueryNames(t)

	for name, isRead := range reads {
		switch listed := slices.Contains(readable, name); {
		case isRead && !listed:
			t.Errorf("%s only reads and is not in ReadOps, so it cannot be used on the "+
				"query-only pool", name)
		case !isRead && listed:
			t.Errorf("%s mutates and is listed in ReadOps; it would reach the ledger "+
				"through ReadQueries, which binds the query-only pool", name)
		}
	}

	for _, name := range readable {
		if _, ok := reads[name]; !ok {
			t.Errorf("ReadOps names %s, which is in no query file", name)
		}
	}
}

// AND THE READ-ONLY HANDLE ITSELF REFUSES A MUTATION.
//
// The companion to the test above, and not a duplicate of it: that one says
// ReadOps is the right SET, this one says the binding underneath actually
// refuses. Together they cover both halves — a mutation cannot be listed, and a
// mutation that somehow reached this handle does not execute.
func TestTheReadOnlyHandleRefusesEveryMutatingQuery(t *testing.T) {
	db := open(t)

	readOnly := ledgerdb.New(readOnlyDBTX{q: db.Reader()})
	reads := readOnlyQueryNames(t)

	value := reflect.ValueOf(readOnly)

	var mutations int

	for _, name := range queryMethodNames(t) {
		if reads[name] {
			continue
		}

		method := value.MethodByName(name)
		if !method.IsValid() {
			t.Fatalf("%s is not a method on the generated *Queries", name)
		}

		mutations++

		if err := callQueryMethod(t, method); !refusedAsReadOnly(err) {
			t.Errorf("%s mutates and was not refused on the read-only handle; got %v",
				name, err)
		}
	}

	if mutations == 0 {
		t.Fatal("no mutating query was exercised, so this proved nothing")
	}
}

// refusedAsReadOnly reports whether a mutation was stopped, by either layer that
// can stop one.
//
// TWO ANSWERS ARE CORRECT, AND REQUIRING ONLY THE FIRST WOULD BAN A LEGAL QUERY.
// An `:exec` mutation reaches readOnlyDBTX.ExecContext and gets billet's
// sentinel. An `UPDATE … RETURNING` declared `:one` is dispatched through
// QueryRowContext, which the adapter forwards, so SQLite refuses it instead --
// "attempt to write a readonly database", code 8, measured. billet has no such
// query today, and a test that failed the day somebody added a correct one is a
// test that refuses correct code.
//
// What both answers share is the property this asserts: the write did not land.
func refusedAsReadOnly(err error) bool {
	if errors.Is(err, errReadOnlyHandle) {
		return true
	}

	// THE EXACT CODE, NOT THE PRIMARY BYTE. Masking with 0xff would also accept
	// every EXTENDED readonly condition -- database moved, recovery, rollback, a
	// directory that cannot be written -- none of which is the query_only refusal
	// this is about, and any of which would let a sick environment satisfy the
	// test while the enforcement path was gone. Measured: a write through the
	// query-only pool returns exactly 8.
	const sqliteReadOnly = 8

	serr, ok := errors.AsType[*sqlite.Error](err)

	return ok && serr.Code() == sqliteReadOnly
}

// readOnlyQueryNames classifies every named query by the first keyword of its
// statement: true for a plain SELECT, false for anything that writes.
//
// A DML STATEMENT WITH RETURNING IS A MUTATION however it is declared, which is
// exactly the case the dispatch-based check could not see.
func readOnlyQueryNames(t *testing.T) map[string]bool {
	t.Helper()

	out := map[string]bool{}

	for _, path := range queryFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		names := sourceAnnotation.FindAllSubmatchIndex(src, -1)
		for i, loc := range names {
			name := string(src[loc[2]:loc[3]])

			end := len(src)
			if i+1 < len(names) {
				end = names[i+1][0]
			}

			// PAST THE REST OF THE ANNOTATION LINE. The regex stops after the
			// name, so what follows is `:one` — which firstKeyword would happily
			// return, classifying every SELECT in the tree as a mutation. That was
			// the first version, and it reported all fourteen reads as writes.
			body := loc[1]
			if nl := bytes.IndexByte(src[body:end], '\n'); nl >= 0 {
				body += nl + 1
			}

			out[name] = classify(t, name, firstKeyword(string(src[body:end])))
		}
	}

	if len(out) == 0 {
		t.Fatal("no queries were classified, so every comparison against this is vacuous")
	}

	return out
}

// firstKeyword returns the first SQL word of a statement, skipping comments.
// classify turns a statement's first keyword into read-or-write, and REFUSES a
// keyword it does not recognise.
//
// FAIL CLOSED ON THE UNKNOWN, because the alternative is a silent default and
// this classification decides whether a query may touch the query-only pool. A
// leading WITH is the shape that will arrive first: a read-only CTE and one
// wrapping an INSERT look identical here, and answering either way without
// looking would be a guess. The remedy is in the message.
func classify(t *testing.T, name, keyword string) bool {
	t.Helper()

	switch keyword {
	case "select":
		return true
	case "insert", "update", "delete", "replace":
		return false
	default:
		t.Errorf("%s starts with %q, which this classification does not recognise. It "+
			"decides whether the query may be listed in ReadOps and reach the query-only "+
			"pool, so extend classify rather than letting it guess -- note that a WITH "+
			"clause can wrap either a SELECT or an INSERT", name, keyword)

		return false
	}
}

func firstKeyword(block string) string {
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}

		// ANY WHITESPACE, not a space: `SELECT\tfoo` would otherwise come back as
		// one token and be refused as an unrecognised keyword.
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}

		return strings.ToLower(strings.TrimSuffix(fields[0], "("))
	}

	return ""
}

// EVERY GENERATED QUERY IS CLAIMED BY EXACTLY ONE HALF.
//
// ReadOps and WriteOps are hand-written, which is what restores the compile-time
// read/write split over a single generated *Queries. The cost of hand-writing
// them is that they can fall behind, and in one direction the compiler does not
// notice: a query added to a .sql file and never listed here still generates,
// still compiles, and is simply dead. That is worth knowing about.
func TestEveryGeneratedQueryIsClaimedByExactlyOneHalf(t *testing.T) {
	claimed := methodNames(reflect.TypeOf((*WriteOps)(nil)).Elem())
	generated := queryMethodNames(t)

	for _, name := range generated {
		if !slices.Contains(claimed, name) {
			t.Errorf("%s is generated and appears in neither ReadOps nor WriteOps, so nothing "+
				"in internal/state calls it", name)
		}
	}

	for _, name := range claimed {
		if !slices.Contains(generated, name) {
			t.Errorf("%s is named in ReadOps or WriteOps and is not generated", name)
		}
	}
}

// QUERY FILES ARE ASCII ONLY.
//
// MEASURED, NOT STYLISTIC. sqlc's SQLite engine rewrites @named parameters into
// numbered placeholders by slicing the statement at a byte offset, and a
// multi-byte character anywhere earlier in the file shifts every offset after
// it. One em dash in a comment turned `force_destroy` into `roy` and `VALUES`
// into `VALU`; the diagnostic is an ANTLR parse error naming neither the file
// nor the character, and it appears on queries far from the one that carries the
// dash. It needs BOTH halves — the same em dash with positional `?` generates
// cleanly — which is why the rule is "these files are ASCII" rather than
// something about dashes near parameters.
//
// This does not apply to internal/state/migrations: those are schema, not
// queries, sqlc does no parameter rewriting on them, and their prose is full of
// em dashes.
func TestQueryFilesAreASCIIOnly(t *testing.T) {
	files := queryFiles(t)

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if utf8.Valid(data) && isASCII(data) {
			continue
		}

		line := 1

		for i, b := range data {
			if b == '\n' {
				line++

				continue
			}

			if b > 0x7f {
				r, _ := utf8.DecodeRune(data[i:])
				t.Errorf("%s line %d has the non-ASCII character %q; sqlc's @parameter rewrite "+
					"slices on byte offsets and a multi-byte character corrupts every statement "+
					"after it. Use ASCII here and put the prose in a Go comment",
					path, line, r)

				break
			}
		}
	}
}

// NO QUERY USES A WILDCARD PROJECTION.
//
// sqlc resolves a star against the schema at CODE-GENERATION time, so the star
// never reaches SQLite — which is exactly the problem. The projection is
// implicit in the source and re-resolves silently whenever the schema changes:
// add a column and every starred query touching that table gains a field in its
// Row type, with nothing in the .sql diff to review; drop one and the projection
// narrows. Since sqlc reads the migration history here, that makes an ordinary
// migration able to change generated code at a distance.
//
// The escape hatch is `-- wildcard-ok: <reason>` on the line or the line above,
// and the reason is mandatory for the same argument nolintlint makes: "0
// violations" has to mean "0 unexplained violations".
func TestNoQueryUsesAWildcardProjection(t *testing.T) {
	// The star must be the whole projection, so count(*) is fine. `(?is)` lets
	// \s+ span newlines, which a line-at-a-time scan would miss.
	patterns := []struct {
		kind string
		re   *regexp.Regexp
	}{
		{"SELECT *", regexp.MustCompile(`(?is)\bSELECT\s+(?:DISTINCT\s+|ALL\s+)?\*`)},
		{"RETURNING *", regexp.MustCompile(`(?is)\bRETURNING\s+\*`)},
		{"alias.*", regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\.\*`)},
	}

	exempt := regexp.MustCompile(`--\s*wildcard-ok:\s*\S`)

	for _, path := range queryFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		lines := strings.Split(string(data), "\n")

		for _, p := range patterns {
			for _, loc := range p.re.FindAllStringIndex(string(data), -1) {
				n := strings.Count(string(data[:loc[0]]), "\n")

				if exempt.MatchString(lines[n]) ||
					(n > 0 && exempt.MatchString(lines[n-1])) {
					continue
				}

				t.Errorf("%s line %d uses %s: %q. An explicit column list keeps a migration "+
					"from changing this query's generated Row type at a distance",
					path, n+1, p.kind, strings.TrimSpace(lines[n]))
			}
		}
	}
}

// AN INTEGER CAST IS SPELLED BIGINT, ON EVERY ENGINE.
//
// ONE QUERY SET IS COMPILED FOR BOTH ENGINES, so a statement whose Go type
// depends on which engine generated it would make the shared adapter a lie —
// and `INTEGER` is exactly that statement. It is int4 on PostgreSQL and typed
// `int32`, while SQLite types the same cast `int64`; `BIGINT` is int64 on both,
// and SQLite has no BIGINT of its own so nothing about that side changes. The
// same is true of a bare parameterised `LIMIT`, which PostgreSQL types int32 for
// the same reason, so those carry a cast too.
//
// THE RULE IS ABOUT THE WIDTH, NOT ABOUT ONE KEYWORD, which is why every
// narrower spelling is named rather than just the one that was here. A query
// added with `AS INT` would fail on PostgreSQL in precisely the way this exists
// to prevent, and would read as fine.
func TestEveryIntegerCastIsBigintSoBothEnginesAgree(t *testing.T) {
	narrow := regexp.MustCompile(`(?i)\bAS\s+(INTEGER|INT|INT2|INT4|SMALLINT)\s*\)`)
	uncastLimit := regexp.MustCompile(`(?i)\bLIMIT\s+@`)

	for _, path := range queryFiles(t) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		lines := strings.Split(string(data), "\n")

		for _, loc := range narrow.FindAllIndex(data, -1) {
			n := bytes.Count(data[:loc[0]], []byte("\n"))

			t.Errorf("%s line %d casts to a narrower integer than BIGINT: %q. PostgreSQL types "+
				"that int32 and SQLite types it int64, so the one generated package would "+
				"depend on which engine produced it",
				path, n+1, strings.TrimSpace(lines[n]))
		}

		for _, loc := range uncastLimit.FindAllIndex(data, -1) {
			n := bytes.Count(data[:loc[0]], []byte("\n"))

			t.Errorf("%s line %d has an uncast parameterised LIMIT: %q. Write "+
				"`LIMIT CAST(@x AS BIGINT)`, or PostgreSQL types the parameter int32",
				path, n+1, strings.TrimSpace(lines[n]))
		}
	}
}

// loadGeneratedQueries reads every query constant out of the generated package.
//
// IT PARSES THE GO rather than pattern-matching it, which is not a preference:
// sqlc escapes a backtick occurring in SQL by splitting the literal into
// `...` + "`" + `..., so a regex truncates any query whose text contains one and
// then reports a bogus syntax error for a statement that is perfectly good. A
// guard that cries wolf is a guard somebody deletes.
//
// The glob is *.go rather than *.sql.go, because a future :batch query would
// land in batch.go and a narrower pattern would skip it in silence.
func loadGeneratedQueries(t *testing.T) []generatedQuery {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(generatedDir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", generatedDir, err)
	}

	var out []generatedQuery

	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		fset := token.NewFileSet()

		file, err := parser.ParseFile(fset, filepath.Base(path), src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}

			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}

				sql, ok := stringConstValue(value.Values[0])
				if !ok || !nameAnnotation.MatchString(sql) {
					continue
				}

				out = append(out, generatedQuery{
					Name: nameAnnotation.FindStringSubmatch(sql)[1],
					File: filepath.Base(path),
					SQL:  sql,
				})
			}
		}
	}

	if len(out) == 0 {
		t.Fatalf("no query constants found in %s; every check over them would be vacuous",
			generatedDir)
	}

	return out
}

// sourceQueryNames returns every query named in the .sql definitions.
func sourceQueryNames(t *testing.T) []string {
	t.Helper()

	var out []string

	for _, path := range queryFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		for _, m := range sourceAnnotation.FindAllStringSubmatch(string(src), -1) {
			out = append(out, m[1])
		}
	}

	return out
}

// queryFiles lists the .sql definitions, refusing an empty set.
func queryFiles(t *testing.T) []string {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(queriesDir, "*.sql"))
	if err != nil {
		t.Fatalf("list %s: %v", queriesDir, err)
	}

	if len(paths) == 0 {
		t.Fatalf("no .sql files in %s; this check would be vacuous", queriesDir)
	}

	return paths
}

// queryMethodNames is every generated query method, which is the method set of
// *Queries minus the transaction binder.
func queryMethodNames(t *testing.T) []string {
	t.Helper()

	typ := reflect.TypeOf((*ledgerdb.Queries)(nil))

	var out []string

	for i := range typ.NumMethod() {
		m := typ.Method(i)

		// WithTx binds a transaction and returns *Queries; it issues no SQL, so it
		// is the one method here that is not a query. Detected by shape rather than
		// by name: a query method always ends in error.
		if m.Type.NumOut() == 0 ||
			m.Type.Out(m.Type.NumOut()-1) != reflect.TypeOf((*error)(nil)).Elem() {
			continue
		}

		out = append(out, m.Name)
	}

	if len(out) == 0 {
		t.Fatal("the generated *Queries has no query methods, so this proved nothing")
	}

	return out
}

// methodNames lists an interface's methods.
func methodNames(typ reflect.Type) []string {
	out := make([]string, 0, typ.NumMethod())
	for i := range typ.NumMethod() {
		out = append(out, typ.Method(i).Name)
	}

	return out
}

// callQueryMethod invokes one generated query with zero-value arguments and
// returns its error.
//
// The ARGUMENTS DO NOT MATTER: what is being asked is which code path the call
// takes inside the generated method — QueryContext, or ExecContext — and that is
// decided by the query's kind rather than by its parameters.
func callQueryMethod(t *testing.T, method reflect.Value) error {
	t.Helper()

	typ := method.Type()

	args := make([]reflect.Value, 0, typ.NumIn())
	args = append(args, reflect.ValueOf(t.Context()))

	for i := 1; i < typ.NumIn(); i++ {
		args = append(args, reflect.New(typ.In(i)).Elem())
	}

	out := method.Call(args)

	last := out[len(out)-1]
	if last.IsNil() {
		return nil
	}

	err, ok := last.Interface().(error)
	if !ok {
		t.Fatalf("the last return value is %s, not an error", last.Type())
	}

	return err
}

// stringConstValue evaluates a string constant, including the
// `a` + "b" + `c` form sqlc emits when the SQL contains a backtick.
func stringConstValue(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}

		unquoted, err := strconv.Unquote(e.Value)
		if err != nil {
			return "", false
		}

		return unquoted, true

	case *ast.ParenExpr:
		return stringConstValue(e.X)

	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}

		left, leftOK := stringConstValue(e.X)
		right, rightOK := stringConstValue(e.Y)

		if !leftOK || !rightOK {
			return "", false
		}

		return left + right, true

	default:
		return "", false
	}
}

func isASCII(data []byte) bool {
	for _, b := range data {
		if b > 0x7f {
			return false
		}
	}

	return true
}
