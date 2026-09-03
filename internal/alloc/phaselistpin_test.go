package alloc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TWO PHASE LISTS LIVE IN SQL AS LITERALS, AND THESE PIN THE STATEMENTS THEY SIT
// IN.
//
// Everywhere else in internal/alloc a phase reaches a statement as a named
// PARAMETER filled from its Go constant, so no second spelling can drift. These
// two cannot be written that way: the sets are Go SLICES whose length may change,
// sqlc.slice() is not available on the SQLite engine, and a generated query has
// fixed arity. So the list is written out in the .sql and compared here.
//
// THE WHOLE STATEMENT IS PINNED, NOT THE LIST, AND NOT A CLAUSE OF IT. Two
// weaker versions came first and each was broken in one edit. Comparing the
// extracted `phase IN (...)` alone ignored everything around it: prefixing
// `0 = 1 AND` keeps the list identical and the pin green while the query returns
// nothing at all -- on the drain path that reports no outstanding leases, and
// `billet local down` stops services on that answer. Comparing the extracted
// WHERE fixed that and left the EXTRACTION as the way in, since a bound that
// stops at ORDER BY, LIMIT or a semicolon reads a shorter region as soon as a
// subquery carries one of them, and one that starts at the first WHERE reads a
// subquery's instead of the statement's.
//
// So there is no extraction. The comparison is the entire statement, normalised
// for layout only, against one written out below -- which is also why a changed
// PROJECTION fails here, and should: these two rows are what an operator is shown
// before authorising a force and what a drain reports as still outstanding.

// BOTH DIRECTIONS OF THE LIST MATTER, and the whole-statement comparison keeps
// them: a phase in the Go slice and not in the SQL is, for force-destroy, a lease
// an operator approved destroying that the listing never offers, and for
// quiescence compute a drain never sees. A phase in the SQL and not in the slice
// is the reverse -- ForceTerminate's allowlist reads the slice while the listing
// reads the SQL, so a lease could be offered, destroyed, then refused.
func TestTheForceDestroyStatementIsWhatTheGoSliceSays(t *testing.T) {
	t.Parallel()

	// AN EMPTY FILTER MEANS UNFILTERED, which is the part of this statement most
	// easily narrowed by accident, so both filters are written out. And the host
	// filter reads COALESCE(node, target_node, ''): narrowed to `node`, a force
	// aimed at a host never reaches the leases merely TARGETED at it, which are
	// the ones a stuck launch leaves behind.
	want := normalizeSQL(`
		SELECT id, tier, COALESCE(node, target_node, '') AS node, phase,
		       CAST(COALESCE(CAST(run_id AS TEXT), '') AS TEXT) AS run_id,
		       COALESCE(request_id, 0) AS request_id,
		       CAST(CASE WHEN held_at = '' THEN created_at ELSE held_at END AS TEXT) AS since
		  FROM leases
		 WHERE ` + phaseIn(forceDestroyPhases) + `
		   AND (CAST(@tier AS TEXT) = '' OR tier = CAST(@tier AS TEXT))
		   AND (CAST(@node AS TEXT) = '' OR COALESCE(node, target_node, '') = CAST(@node AS TEXT))
		 ORDER BY CASE WHEN held_at = '' THEN created_at ELSE held_at END, id`)

	if got := statementOf(t, "ListForceDestroyCandidates"); got != want {
		t.Errorf("ListForceDestroyCandidates is no longer the statement this test pins.\n"+
			"forceDestroyPhases and the two filters say:\n  %s\nthe query file says:\n  %s",
			want, got)
	}
}

func TestTheDrainStatementIsWhatTheGoSliceSays(t *testing.T) {
	t.Parallel()

	want := normalizeSQL(`
		SELECT id, tier, COALESCE(node, target_node, '') AS node, phase,
		       CAST(COALESCE(CAST(run_id AS TEXT), '') AS TEXT) AS run_id,
		       CAST(CASE WHEN held_at = '' THEN created_at ELSE held_at END AS TEXT) AS since,
		       deregistered
		  FROM leases
		 WHERE ` + phaseIn(quiescencePhases) + `
		 ORDER BY CASE WHEN held_at = '' THEN created_at ELSE held_at END, id`)

	if got := statementOf(t, "ListOutstandingLeases"); got != want {
		t.Errorf("ListOutstandingLeases is no longer the statement this test pins.\n"+
			"quiescencePhases says:\n  %s\nthe query file says:\n  %s\n"+
			"A drain that sees less than it should reports no outstanding leases, and "+
			"`billet local down` stops services on that answer", want, got)
	}
}

// phaseIn renders a Go phase slice as the SQL predicate it must appear as.
func phaseIn(phases []Phase) string {
	if len(phases) == 0 {
		panic("a phase list this test pins is empty, so the comparison would be vacuous")
	}

	quoted := make([]string, 0, len(phases))
	for _, p := range phases {
		quoted = append(quoted, "'"+string(p)+"'")
	}

	return "phase IN (" + strings.Join(quoted, ",") + ")"
}

// statementOf returns one named query's SQL from the query file, normalised.
//
// SCOPED TO THE NAMED QUERY, because the file holds several and reading the wrong
// one is the whole failure mode this test exists to prevent.
func statementOf(t *testing.T, query string) string {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("..", "state", "queries", "forcedestroy_leases.sql"))
	if err != nil {
		t.Fatalf("read forcedestroy_leases.sql: %v", err)
	}

	body := string(src)

	at := strings.Index(body, "-- name: "+query+" ")
	if at < 0 {
		t.Fatalf("forcedestroy_leases.sql has no %s; if the query was renamed, move this "+
			"pin with it rather than deleting it", query)
	}

	rest := body[at:]
	if end := strings.Index(rest[1:], "\n-- name: "); end >= 0 {
		rest = rest[:end+1]
	}

	// EVERY COMMENT LINE GOES, the `-- name:` header included: the prose above one
	// of these statements is long and is meant to be edited freely.
	var sql strings.Builder

	for _, line := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			sql.WriteString(line)
			sql.WriteString("\n")
		}
	}

	return normalizeSQL(strings.TrimSuffix(strings.TrimSpace(sql.String()), ";"))
}

// EVERY PIN IN THIS PACKAGE RESTS ON normalizeSQL, so it is tested in BOTH
// DIRECTIONS rather than only through the pins that use it.
//
// One direction alone is worse than useless here. A normaliser that separates
// everything keeps every pin green and fails on the next innocent reflow of a
// query file, which is how a check gets deleted; one that equates everything
// passes a pin over a statement that does something else. Neither shows up in a
// pin that happens to be green.
//
// THE CASES ARE THE INTERACTIONS, not the features: a comment marker inside a
// literal, a quote inside a comment, a comment with no whitespace around it, the
// doubled-quote escape, a `-` that is subtraction and a `/` that is division, and
// an unterminated construct. Those are where a hand-written tokeniser goes wrong,
// and three earlier versions of this function went wrong in exactly that region.
func TestEquivalentStatementsNormaliseTogether(t *testing.T) {
	t.Parallel()

	for _, c := range [][2]string{
		// Whitespace and separator layout, which is all this may erase.
		{"SELECT a,\n   b FROM t", "SELECT a, b FROM t"},
		{"WHERE x IN ( 'a', 'b' )", "WHERE x IN ('a','b')"},

		// FORM FEED AND VERTICAL TAB are whitespace to SQLite's tokeniser, and this
		// is the case that makes isSQLSpace's two extra bytes real rather than
		// asserted: without them the mutant that removes one survives.
		{"SELECT\f1, \va FROM t", "SELECT 1, a FROM t"},

		// A COMMENT IS WHITESPACE TO SQLITE, including with none around it -- which
		// is why one is replaced by a separator rather than simply dropped.
		{"SELECT a -- note\nFROM t", "SELECT a FROM t"},
		{"SELECT a/*note*/FROM t", "SELECT a FROM t"},
		{"SELECT a /* n */ , b FROM t", "SELECT a, b FROM t"},
		{"SELECT a\n-- whole line\nFROM t", "SELECT a FROM t"},

		// A comment marker inside a literal is not a comment.
		{"WHERE x = 'a -- b'  AND y = 1", "WHERE x = 'a -- b' AND y = 1"},
		{"WHERE x = 'a /* b */ c'   AND y = 1", "WHERE x = 'a /* b */ c' AND y = 1"},

		// A quote inside a comment does not open a literal.
		{"SELECT a -- it's fine\nFROM t", "SELECT a FROM t"},

		// The doubled-quote escape, and the empty literal that looks like one.
		{"WHERE x = 'a''b'   AND y = 1", "WHERE x = 'a''b' AND y = 1"},
		{"WHERE x = ''  AND y = 1", "WHERE x = '' AND y = 1"},

		// Not comments: subtraction and division.
		{"SELECT a - 1,  b / 2 FROM t", "SELECT a - 1, b / 2 FROM t"},
	} {
		if got, want := normalizeSQL(c[0]), normalizeSQL(c[1]); got != want {
			t.Errorf("two spellings of one statement were separated:\n  %q\n  %q", got, want)
		}
	}
}

func TestDifferentStatementsStayApart(t *testing.T) {
	t.Parallel()

	for _, c := range [][2]string{
		// THE NEWLINE IS THE ONLY THING ENDING A LINE COMMENT, so treating it as
		// layout puts a statement with a FROM clause and one whose FROM clause is
		// commented out onto the same string.
		{"SELECT 1 -- x\nFROM leases", "SELECT 1 -- x FROM leases"},

		// Whitespace inside each of SQLite's four quoting forms.
		{`SELECT "a  b" FROM t`, `SELECT "a b" FROM t`},
		{"SELECT `a  b` FROM t", "SELECT `a b` FROM t"},
		{`SELECT [a  b] FROM t`, `SELECT [a b] FROM t`},
		{`WHERE x = 'a  b'`, `WHERE x = 'a b'`},

		// The separator rules, which are the ones this deliberately applies.
		{`SELECT "a, b" FROM t`, `SELECT "a,b" FROM t`},
		{`WHERE x = 'a, b'`, `WHERE x = 'a,b'`},
		{`WHERE x = '( y'`, `WHERE x = '(y'`},

		// A quoted IDENTIFIER is not a quoted VALUE.
		{"SELECT `name` FROM t", `SELECT 'name' FROM t`},

		// An unterminated construct must not read as a terminated one.
		{"WHERE x = 'a", "WHERE x = 'a'"},
		{"SELECT a /* c FROM t", "SELECT a FROM t"},
	} {
		if normalizeSQL(c[0]) == normalizeSQL(c[1]) {
			t.Errorf("two statements that mean different things both normalise to %q:\n"+
				"  %q\n  %q", normalizeSQL(c[0]), c[0], c[1])
		}
	}
}

// normalizeSQL reduces a statement to its shape, so a reflow of the .sql cannot
// fail a pin and a changed statement cannot pass one. BOTH SIDES go through it,
// which is what lets the expectations above be written the way the query file
// writes them rather than matching its layout character for character.
//
// LAYOUT ONLY, AND ONLY BETWEEN TOKENS. It collapses runs of whitespace and
// removes the space after a comma or an open bracket and before a close bracket
// -- all insignificant to SQLite between tokens, and all significant INSIDE a
// quoted string or a quoted identifier, where `'a, b'` and `'a,b'` are different
// values that a blind normaliser maps to one. Measured: the blind form collapses
// four such pairs.
//
// ALL FOUR OF SQLITE'S QUOTING FORMS ARE TRACKED, not just the single quote --
// `"x"`, `[x]` and a backtick are quoted IDENTIFIERS and may contain anything.
// Resting on "no statement here has one today" is the same bet the previous three
// versions of this made and lost; a normaliser that knows the grammar does not
// have to be re-audited when a statement gains one.
//
// AND A COMMENT IS REMOVED RATHER THAN COLLAPSED, because collapsing one changes
// what executes: the newline that ENDS a `--` comment is the only thing
// separating `SELECT 1 -- x` + `FROM leases` from `SELECT 1 -- x FROM leases`,
// and treating that newline as layout maps a two-clause statement onto a
// one-clause one. Removing the comment leaves `SELECT 1 FROM leases` against
// `SELECT 1`, which is the difference that is actually there.
//
// A DOUBLED QUOTE IS SQLITE'S ESCAPE and needs no special case: the first closes
// the literal and the second opens another, so the parity comes out the same.
// Brackets do not nest and have no escape, which is why `]` simply closes.
//
// ONE FALSE POSITIVE IS LEFT STANDING DELIBERATELY, and the reason is the whole
// argument for not going further: a comment removed between an OPERATOR and its
// operand leaves a space the other spelling has not got, so `1+/*c*/2` and `1+2`
// are reported as different when SQLite reads them the same. Closing it means
// dropping the space beside operators as well as beside brackets -- and that
// WELDS `x - -y` into `x--y`, which is a COMMENT. A false positive costs somebody
// updating a pin they meant to leave alone; that collision would pass a statement
// whose second operand has vanished. The pins are for meaning, so the error goes
// in the direction that cannot be wrong about it.
func normalizeSQL(sql string) string {
	var (
		out     strings.Builder
		last    byte
		closer  byte // the delimiter ending the quoted run we are inside, or 0
		pending bool // whitespace seen since the last emitted byte
	)

	for i := 0; i < len(sql); i++ {
		c := sql[i]

		if closer != 0 {
			out.WriteByte(c)

			if c == closer {
				closer = 0
			}

			last = c

			continue
		}

		// A COMMENT COUNTS AS THE WHITESPACE THAT SURROUNDS IT, so removing one
		// cannot weld the tokens on either side together.
		if n := commentEnd(sql, i); n > i {
			pending = true
			i = n - 1

			continue
		}

		if isSQLSpace(c) {
			pending = true

			continue
		}

		// NO SPACE BEFORE A COMMA OR A CLOSE BRACKET, AND NONE AFTER A COMMA OR AN
		// OPEN BRACKET. That is the whole rule, and it is what lets the expectations
		// above be written with the spacing a person uses while the query file keeps
		// the spacing SQL reads well in. Everything else keeps its single space, so
		// `AS TEXT) AS since` stays two tokens rather than being run together.
		if pending && last != 0 && c != ',' && c != ')' && last != '(' && last != ',' {
			out.WriteByte(' ')
		}

		pending = false

		out.WriteByte(c)

		closer = quoteCloser(c)
		last = c
	}

	return out.String()
}

// commentEnd returns the index just past a SQL comment starting at i, or i.
//
// An unterminated block comment runs to the end, which is what SQLite does with
// one too.
func commentEnd(sql string, i int) int {
	if strings.HasPrefix(sql[i:], "--") {
		if n := strings.IndexByte(sql[i:], '\n'); n >= 0 {
			return i + n
		}

		return len(sql)
	}

	if strings.HasPrefix(sql[i:], "/*") {
		if n := strings.Index(sql[i+2:], "*/"); n >= 0 {
			return i + 2 + n + 2
		}

		return len(sql)
	}

	return i
}

// isSQLSpace reports whether a byte is whitespace to SQLite.
//
// FORM FEED AND VERTICAL TAB ARE IN THE LIST because SQLite's tokeniser accepts
// them, not because a query file here has one -- the same reason every quoting
// form is tracked rather than the one these statements use.
func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

// quoteCloser returns the delimiter that ends a quoted run this byte opens, or 0.
func quoteCloser(c byte) byte {
	switch c {
	case '\'', '"', '`':
		return c
	case '[':
		return ']'
	}

	return 0
}
