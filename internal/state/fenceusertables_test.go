package state

import (
	"database/sql"
	"slices"
	"testing"
)

// A TABLE userTables OMITS IS A TABLE PeekLedger NEVER COUNTS ROWS IN, which makes
// a populated ledger read as pristine and lets a restore replace a live
// deployment's capacity record. So both halves of its predicate matter.
//
// `_` IS A LIKE WILDCARD, so `name NOT LIKE 'sqlite_%'` excluded `sqliteX…` as
// well as SQLite's own reserved `sqlite_…` names. billet has no table named that
// way, which is why the predicate could be wrong for as long as it was — so this
// asserts the property against names SQLite will actually accept rather than
// against the names billet happens to use.
//
// BOTH FIXTURES ARE REAL TABLES, and the first version of this test got that
// wrong: it counted `sqlite_autoindex_*` entries as evidence for the exclusion
// half, and those are INDEXES. userTables already filters `type = 'table'`, so
// they could never have appeared in its result whatever the name predicate said —
// deleting that predicate entirely would have passed. `sqlite_sequence` is the
// reserved TABLE SQLite creates for an AUTOINCREMENT column, which is a fixture
// this test can actually make.
func TestUserTablesExcludesOnlyTheReservedPrefix(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		// A legal name the old pattern's wildcard swallowed.
		if _, err := tx.ExecContext(ctx, `CREATE TABLE sqliteXledger (a TEXT) STRICT`); err != nil {
			return err
		}

		// AUTOINCREMENT is what makes SQLite create the reserved sqlite_sequence
		// TABLE. Nothing in billet's schema uses it; this exists only so there is
		// a reserved table to exclude.
		if _, err := tx.ExecContext(ctx,
			`CREATE TABLE probe_counter (id INTEGER PRIMARY KEY AUTOINCREMENT, a TEXT)`); err != nil {
			return err
		}

		_, err := tx.ExecContext(ctx, `INSERT INTO probe_counter (a) VALUES ('x')`)

		return err
	}); err != nil {
		t.Fatalf("build the fixtures: %v", err)
	}

	// THE FIXTURE HAS TO EXIST, AS A TABLE, or the exclusion below checks nothing.
	// This is the assertion whose absence made the first version vacuous.
	var kind string
	if err := db.Reader().QueryRowContext(ctx,
		`SELECT type FROM sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&kind); err != nil {
		t.Fatalf("AUTOINCREMENT did not produce a sqlite_sequence entry, so there is no "+
			"reserved TABLE to exclude: %v", err)
	}

	if kind != "table" {
		t.Fatalf("sqlite_sequence is a %q, not a table, so userTables would filter it on "+
			"type rather than on name and this test would prove nothing", kind)
	}

	tables, err := userTables(ctx, db.Reader())
	if err != nil {
		t.Fatalf("userTables: %v", err)
	}

	if !slices.Contains(tables, "sqliteXledger") {
		t.Errorf("userTables omitted sqliteXledger, a legal user table: %v\n"+
			"`_` is a LIKE wildcard, so 'sqlite_%%' matches it. PeekLedger would never "+
			"count its rows, and a populated ledger would read as pristine.", tables)
	}

	if slices.Contains(tables, "sqlite_sequence") {
		t.Errorf("userTables returned the reserved table sqlite_sequence: %v", tables)
	}
}

// WHAT THE DRIVER RETURNS IS NOT BILLET'S TO ASSUME, and rowsOf's comparison
// depends on it: renderCell tags each cell with its storage class so two values
// SQLite stores differently cannot render alike, and the tags are only truthful
// while these are the types that arrive.
//
// TEXT ARRIVING AS A string IS THE ONE THAT MATTERS. As []byte every text value
// would render `blob:…`, and the tag would be actively misleading rather than
// merely coarse — a difference a reader would trust and a comparison would not
// see.
//
// Pinned here rather than left as a measurement in a comment, for the reason the
// runner-group validator is pinned: a rule that encodes an assumption about code
// billet does not own belongs in a test that runs.
func TestTheDriverReturnsOneGoTypePerStorageClass(t *testing.T) {
	db := open(t)
	ctx := t.Context()

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`CREATE TABLE probe (t TEXT, i INTEGER, r REAL, b BLOB, n TEXT) STRICT`); err != nil {
			return err
		}

		_, err := tx.ExecContext(ctx,
			`INSERT INTO probe (t, i, r, b, n) VALUES ('one', 1, 1.0, x'31', NULL)`)

		return err
	}); err != nil {
		t.Fatalf("seed the probe: %v", err)
	}

	rows, err := db.Reader().QueryContext(ctx, `SELECT t, i, r, b, n FROM probe`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}

	defer rows.Close()

	if !rows.Next() {
		t.Fatal("the probe row is missing, so nothing below was checked")
	}

	cells := make([]any, 5)
	into := make([]any, 5)

	for i := range cells {
		into[i] = &cells[i]
	}

	if err := rows.Scan(into...); err != nil {
		t.Fatalf("scan: %v", err)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}

	for _, tc := range []struct {
		column string
		cell   any
		want   string
	}{
		{"TEXT", cells[0], `text:"one"`},
		{"INTEGER", cells[1], "int:1"},
		{"REAL", cells[2], "real:1"},
		{"BLOB", cells[3], "blob:31"},
		{"NULL", cells[4], "NULL"},
	} {
		got := renderCell(t, tc.cell)
		if got != tc.want {
			t.Errorf("a %s cell arrived as %T and rendered %q, want %q\n"+
				"renderCell's tags are only truthful while these are the types the driver "+
				"returns; a new one makes the comparison in rowsOf coarser than it reads.",
				tc.column, tc.cell, got, tc.want)
		}
	}
}
