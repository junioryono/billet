package state

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// THE TWO TIMELINES ARE THE ONE PLACE BILLET CARRIES THE SAME IDEA TWICE, and
// everything in this file exists to stop the two copies drifting.
//
// DDL is not portable — STRICT, an INTEGER column with CHECK (x IN (0,1))
// standing in for a boolean, and the table rebuilds that only ever worked around
// SQLite's ALTER limits are all SQLite's own spelling — so a second engine needs
// a second set of schema statements. The QUERIES are not duplicated: one
// directory, compiled once, executing on both.
//
// WHAT MAKES A DUPLICATED TIMELINE SURVIVABLE IS THAT IT IS DERIVED. Every
// PostgreSQL migration is its SQLite twin with four declared substitutions applied
// and nothing else changed, so the comparison below is exact rather than a review by
// eye — which is what "43 files, 121 statements" would otherwise need, on every
// change, forever.

// pgTypeRewrite is one type spelling that differs between the engines.
//
// ORDERED AND EXPLICIT rather than a general SQL rewriter: the point is that the
// list is short enough to read and to argue with. A translation nobody can state
// in a table is a translation nobody can check.
type pgTypeRewrite struct {
	// why is not decoration. Each of these is a fact about one of the two
	// engines, and the reason is what a reader needs to decide whether a new
	// entry belongs here or is a schema divergence wearing a substitution's
	// clothes.
	why  string
	from string
	to   string
}

var pgTypeRewrites = []pgTypeRewrite{
	{
		why:  "spelling; the types are the same",
		from: "TEXT",
		to:   "text",
	},
	{
		// THE ONE THAT IS NOT COSMETIC. PostgreSQL's INTEGER is int4, and sqlc
		// types it int32, while SQLite's INTEGER is a 64-bit value that sqlc types
		// int64. bigint is what SQLite's INTEGER actually is, so this preserves the
		// column rather than changing it. The same rule governs the query set — see
		// TestEveryIntegerCastIsBigintSoBothEnginesAgree.
		why:  "SQLite's INTEGER is 64-bit; PostgreSQL's is int4, so bigint is the same column",
		from: "INTEGER",
		to:   "bigint",
	},
}

// strictSuffix is the third declared difference. Anchored to the END of the
// statement, which is also what makes it safe: a `) STRICT` inside a string
// literal cannot be the last thing in a statement, because the literal has to be
// closed after it.
var strictSuffix = regexp.MustCompile(`\)\s+STRICT\z`)

// terminator is the fourth, and it is a fact about sqlc rather than about
// PostgreSQL.
//
// billet's migration format has no semicolons — a statement is delimited by its
// markers, and the bytes between them are executed as they stand. sqlc's
// PostgreSQL parser reads a schema FILE rather than a statement, so without a
// terminator it runs consecutive statements together and reports `syntax error
// at or near "ALTER"` on the second one. Measured on v1.31.1: nine of the 43
// files failed to parse until this was added.
const terminator = ";"

// translateToPostgres is the declared translation, and the ORACLE the comparison
// below is written against.
//
// IT IS LEXICALLY AWARE, AND A REGEXP IS NOT ENOUGH, which was worth the forty
// lines below. A global \bINTEGER\b replacement rewrites the word wherever it
// occurs — inside a string literal, inside a quoted identifier, inside a comment
// — so a future data migration reading `UPDATE t SET kind = 'INTEGER'` would
// silently store a DIFFERENT VALUE on PostgreSQL, and the comparison this
// function serves would bless it, because it would be comparing the corruption
// against itself. The same argument applies to the case: matching only the
// uppercase spelling means a migration written `integer` translates to itself,
// leaves PostgreSQL with an int4 column where SQLite has a 64-bit one, and
// passes.
//
// So the scan tracks the three contexts SQL has and rewrites only outside them,
// matching a type name case-insensitively and only where it stands as a whole
// word. None of today's 121 statements contains a type name in any of those
// contexts, so this changed no published byte — it is here for the migration
// that has not been written yet.
func translateToPostgres(stmt string) string {
	var b strings.Builder

	b.Grow(len(stmt))

	for i := 0; i < len(stmt); {
		// A quoted region is copied through whole. An escaped quote inside one is
		// SQL's doubled '' — which this handles without a special case, because the
		// first of the pair closes the region and the second immediately opens
		// another, so the interior is never treated as code either way.
		if q := stmt[i]; q == '\'' || q == '"' {
			end := strings.IndexByte(stmt[i+1:], q)
			if end < 0 {
				// UNTERMINATED, so there is no code left to translate. Copying the
				// remainder verbatim is the conservative answer: such a statement
				// cannot apply on either engine, and the comparison will report the
				// difference rather than this quietly inventing one.
				b.WriteString(stmt[i:])

				break
			}

			b.WriteString(stmt[i : i+1+end+1])
			i += 1 + end + 1

			continue
		}

		if strings.HasPrefix(stmt[i:], "--") {
			end := strings.IndexByte(stmt[i:], '\n')
			if end < 0 {
				b.WriteString(stmt[i:])

				break
			}

			b.WriteString(stmt[i : i+end+1])
			i += end + 1

			continue
		}

		if strings.HasPrefix(stmt[i:], "/*") {
			end := strings.Index(stmt[i+2:], "*/")
			if end < 0 {
				b.WriteString(stmt[i:])

				break
			}

			b.WriteString(stmt[i : i+2+end+2])
			i += 2 + end + 2

			continue
		}

		if word, ok := typeRewriteAt(stmt, i); ok {
			b.WriteString(word.to)
			i += len(word.from)

			continue
		}

		b.WriteByte(stmt[i])
		i++
	}

	return strictSuffix.ReplaceAllString(b.String(), ")") + terminator
}

// typeRewriteAt reports the rewrite whose name stands as a whole word at i.
func typeRewriteAt(stmt string, i int) (pgTypeRewrite, bool) {
	if i > 0 && isSQLWordByte(stmt[i-1]) {
		return pgTypeRewrite{}, false
	}

	for _, r := range pgTypeRewrites {
		end := i + len(r.from)
		if end > len(stmt) || !strings.EqualFold(stmt[i:end], r.from) {
			continue
		}

		if end < len(stmt) && isSQLWordByte(stmt[end]) {
			continue
		}

		return r, true
	}

	return pgTypeRewrite{}, false
}

func isSQLWordByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// EVERY POSTGRESQL MIGRATION IS ITS SQLITE TWIN, TRANSLATED.
//
// This is the guard that makes two timelines safe to have. It catches, by name:
// a SQLite migration added with no PostgreSQL twin; a twin whose statements have
// been edited on one side only; a twin with a different number of statements or
// with them in a different order; and a twin claiming a version or a name the
// other does not.
//
// IT COMPARES STATEMENT BYTES, NOT SCHEMAS, and that is the stronger of the two
// questions for this failure mode. Two files can produce the same schema and
// still disagree about what they DO — an UPDATE that backfills a column on one
// engine and not the other leaves both catalogues identical and the data
// different. A schema comparison cannot see that; this can.
func TestEveryPostgresMigrationIsItsSQLiteTwinTranslated(t *testing.T) {
	t.Parallel()

	sqlite := loadedTimeline(t, sqliteTimeline)
	postgres := loadedTimeline(t, pgTimeline)

	byVersion := make(map[int]migration, len(postgres))
	for _, m := range postgres {
		byVersion[m.Version] = m
	}

	for _, want := range sqlite {
		got, ok := byVersion[want.Version]
		if !ok {
			t.Errorf("migration %d (%s) has no PostgreSQL twin. Add %s/%04d_%s.sql containing:\n\n%s",
				want.Version, want.Name, pgMigrationDir, want.Version, want.Name,
				renderTranslated(want))

			continue
		}

		if got.Name != want.Name {
			t.Errorf("migration %d is %q on the SQLite timeline and %q on the PostgreSQL one; "+
				"a version is one identity on both", want.Version, want.Name, got.Name)
		}

		if len(got.Stmts) != len(want.Stmts) {
			t.Errorf("migration %d (%s) has %d statements on the SQLite timeline and %d on the "+
				"PostgreSQL one; the twin must carry the same statements in the same order",
				want.Version, want.Name, len(want.Stmts), len(got.Stmts))

			continue
		}

		for i, stmt := range want.Stmts {
			translated := translateToPostgres(stmt)
			if got.Stmts[i] == translated {
				continue
			}

			t.Errorf("migration %d (%s) statement %d is not its SQLite twin translated.\n"+
				"want:\n%s\n\ngot:\n%s",
				want.Version, want.Name, i, translated, got.Stmts[i])
		}
	}

	// THE OTHER DIRECTION. A PostgreSQL migration whose SQLite twin was deleted
	// still parses, still applies, and is invisible to every check above.
	for _, m := range postgres {
		if !slices.ContainsFunc(sqlite, func(s migration) bool { return s.Version == m.Version }) {
			t.Errorf("PostgreSQL migration %d (%s) has no SQLite twin; the timelines have "+
				"diverged", m.Version, m.Name)
		}
	}
}

// renderTranslated prints a whole migration's translated statements, so the
// failure above is something to paste rather than something to work out.
func renderTranslated(m migration) string {
	var b strings.Builder

	for _, stmt := range m.Stmts {
		fmt.Fprintf(&b, "%s\n%s\n%s\n\n", stmtOpenMarker, translateToPostgres(stmt), stmtCloseMarker)
	}

	return b.String()
}

// THE TRANSLATION IS ITSELF TESTED, IN BOTH DIRECTIONS.
//
// It is the oracle the comparison above trusts, so a bug in it would not fail a
// test — it would make the comparison agree with a mistake. The pairs are chosen
// for the interactions rather than for the features: a type name inside an
// identifier, inside a string literal, and STRICT in a position that is not the
// end of the statement.
func TestTheTranslationChangesExactlyWhatItClaimsTo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "types and STRICT",
			in:   "CREATE TABLE t (\n\ta TEXT NOT NULL,\n\tb INTEGER NOT NULL\n) STRICT",
			want: "CREATE TABLE t (\n\ta text NOT NULL,\n\tb bigint NOT NULL\n);",
		},
		{
			name: "a table with no STRICT is left alone but for its types",
			in:   "CREATE TABLE t (\n\ta TEXT\n)",
			want: "CREATE TABLE t (\n\ta text\n);",
		},
		{
			// WORD-BOUNDED, so a column called `context` or a table called
			// `integers` is not rewritten. Without that this is the mistake that
			// produces `conbigintt`.
			name: "a type name inside an identifier is not a type",
			in:   "ALTER TABLE contexts ADD COLUMN integer_ish TEXT NOT NULL DEFAULT ''",
			want: "ALTER TABLE contexts ADD COLUMN integer_ish text NOT NULL DEFAULT '';",
		},
		{
			name: "a CHECK constraint's contents survive",
			in:   "ALTER TABLE t ADD COLUMN c INTEGER NOT NULL DEFAULT 0 CHECK (c IN (0, 1))",
			want: "ALTER TABLE t ADD COLUMN c bigint NOT NULL DEFAULT 0 CHECK (c IN (0, 1));",
		},
		{
			name: "an UPDATE carries no types and is untouched",
			in:   "UPDATE leases SET guest_os = 'macos' WHERE macos_slot = 1",
			want: "UPDATE leases SET guest_os = 'macos' WHERE macos_slot = 1;",
		},
		{
			name: "a RENAME COLUMN is untouched",
			in:   "ALTER TABLE leases RENAME COLUMN job_id TO request_id",
			want: "ALTER TABLE leases RENAME COLUMN job_id TO request_id;",
		},
		{
			// THE ONE THAT WOULD SILENTLY CHANGE DATA. A global replacement
			// rewrites the word wherever it occurs, so this UPDATE would store
			// 'bigint' on PostgreSQL and 'INTEGER' on SQLite — and the derivation
			// test would compare the corruption against itself and pass.
			name: "a type name inside a string literal is data, not a type",
			in:   "UPDATE t SET kind = 'INTEGER' WHERE other = 'TEXT'",
			want: "UPDATE t SET kind = 'INTEGER' WHERE other = 'TEXT';",
		},
		{
			name: "a doubled quote does not end the literal early",
			in:   "UPDATE t SET note = 'it''s INTEGER' WHERE a = 1",
			want: "UPDATE t SET note = 'it''s INTEGER' WHERE a = 1;",
		},
		{
			name: "a type name inside a quoted identifier is a name",
			in:   `ALTER TABLE t ADD COLUMN "INTEGER" TEXT NOT NULL DEFAULT ''`,
			want: `ALTER TABLE t ADD COLUMN "INTEGER" text NOT NULL DEFAULT '';`,
		},
		{
			// The migrations put prose INSIDE statements — migration 1's CREATE
			// TABLE carries three lines of it — and rewriting a word there would
			// change published bytes to say something the SQLite file does not.
			name: "a line comment inside a statement is prose",
			in:   "CREATE TABLE t (\n\t-- an INTEGER column, said in prose\n\ta INTEGER\n) STRICT",
			want: "CREATE TABLE t (\n\t-- an INTEGER column, said in prose\n\ta bigint\n);",
		},
		{
			name: "a block comment is prose too",
			in:   "CREATE TABLE t (\n\t/* INTEGER */\n\ta INTEGER\n)",
			want: "CREATE TABLE t (\n\t/* INTEGER */\n\ta bigint\n);",
		},
		{
			// MATCHED CASE-INSENSITIVELY. Uppercase-only matching leaves a
			// lowercase spelling untranslated, which gives PostgreSQL an int4
			// column where SQLite has a 64-bit one — and passes, because the twin
			// then matches its own untranslated self.
			name: "a lowercase type spelling is still a type",
			in:   "ALTER TABLE t ADD COLUMN c integer NOT NULL DEFAULT 0",
			want: "ALTER TABLE t ADD COLUMN c bigint NOT NULL DEFAULT 0;",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := translateToPostgres(tc.in); got != tc.want {
				t.Errorf("translateToPostgres:\ngot:  %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// STRICT IS ANCHORED TO THE END OF THE STATEMENT, and that is the one
// substitution whose position matters.
//
// Its own test because the anchor is what stops it eating a `) STRICT` that is
// not the table's closing paren — which cannot occur in today's 121 statements
// and is exactly the kind of thing a future migration introduces without anyone
// thinking about this file.
func TestTheStrictSubstitutionOnlyStripsTheClosingParen(t *testing.T) {
	t.Parallel()

	const in = "CREATE TABLE t (\n\ta TEXT DEFAULT ') STRICT'\n) STRICT"
	const want = "CREATE TABLE t (\n\ta text DEFAULT ') STRICT'\n);"

	if got := translateToPostgres(in); got != want {
		t.Errorf("translateToPostgres:\ngot:  %q\nwant: %q", got, want)
	}
}

// EVERY TIMELINE DECLARES THE SAME VERSIONS AND THE SAME NAMES.
//
// LatestSchemaVersion is published in the release manifest as a single int, and
// two binaries compare it across an upgrade without either knowing what backend
// the other was configured for. One number can only describe the binary while
// this holds; without it the upgrade fence compares two different scales, and
// the failure is a control plane that passes the fence, stops, and then cannot
// open the ledger it inherited.
//
// CHECKSUMS ARE NOT COMPARED, and that is not an omission: they are over one
// engine's own statement bytes and are meant to differ. Nothing anywhere
// compares a checksum across timelines.
func TestBothTimelinesDeclareTheSameVersionsAndNames(t *testing.T) {
	t.Parallel()

	sqlite := loadedTimeline(t, sqliteTimeline)
	postgres := loadedTimeline(t, pgTimeline)

	identity := func(set []migration) []string {
		out := make([]string, 0, len(set))
		for _, m := range set {
			out = append(out, fmt.Sprintf("%d:%s", m.Version, m.Name))
		}

		return out
	}

	if got, want := identity(postgres), identity(sqlite); !slices.Equal(got, want) {
		t.Errorf("the timelines declare different migrations:\n  sqlite:   %v\n  postgres: %v",
			want, got)
	}

	if got := LatestSchemaVersion(); got != pgTimeline.latest() {
		t.Errorf("LatestSchemaVersion() is %d and the PostgreSQL timeline's highest is %d; "+
			"the number published in the release manifest has to describe the binary rather "+
			"than one of its backends", got, pgTimeline.latest())
	}
}

// The PostgreSQL twin of TestEveryMigrationFileIsEmbedded. A file the embed
// pattern does not match simply never runs, and every other test passes because
// the schema it expects was built by the migrations that did.
func TestEveryPostgresMigrationFileIsEmbedded(t *testing.T) {
	t.Parallel()

	onDisk, err := os.ReadDir(pgMigrationDir)
	if err != nil {
		t.Fatalf("read %s: %v", pgMigrationDir, err)
	}

	var want []string

	for _, e := range onDisk {
		if e.IsDir() {
			t.Errorf("%s/%s is a directory; the migration directory holds .sql files only",
				pgMigrationDir, e.Name())

			continue
		}

		if e.Name() == "README.md" {
			continue
		}

		if !strings.HasSuffix(e.Name(), ".sql") {
			t.Errorf("%s/%s is in the migration directory and go:embed skips it silently; "+
				"if it is a migration, name it <version>_<name>.sql, and if it is not, it "+
				"does not belong here", pgMigrationDir, e.Name())

			continue
		}

		want = append(want, e.Name())
	}

	embedded, err := pgMigrationFS.ReadDir(pgMigrationDir)
	if err != nil {
		t.Fatalf("read the embedded %s: %v", pgMigrationDir, err)
	}

	var got []string
	for _, e := range embedded {
		got = append(got, e.Name())
	}

	slices.Sort(want)
	slices.Sort(got)

	if !slices.Equal(want, got) {
		t.Errorf("the embedded PostgreSQL migration set is not the directory:\n"+
			"  on disk:  %v\n  embedded: %v", want, got)
	}

	if pgTimeline.loadErr != nil {
		t.Errorf("the embedded PostgreSQL migration set did not load: %v", pgTimeline.loadErr)
	} else if len(got) != len(pgTimeline.migrations) {
		t.Errorf("%d files embedded but %d migrations parsed",
			len(got), len(pgTimeline.migrations))
	}
}
