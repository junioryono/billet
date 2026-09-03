package a

import (
	"context"
	"database/sql"
	"net/url"
)

// EVERY EXPECTATION BELOW IS A CALL THE ANALYZER MUST FIND, and the cases after
// them are shapes it must stay quiet about. Both halves are here because an
// analyzer that stopped detecting would report zero, which reads exactly like a
// clean tree.

func onADB(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `INSERT INTO leases (id) VALUES (?)`, "x") // want `ExecContext executes SQL from Go`

	return err
}

func onATx(ctx context.Context, tx *sql.Tx) error {
	row := tx.QueryRowContext(ctx, `SELECT epoch FROM leases WHERE id = ?`, "x") // want `QueryRowContext executes SQL from Go`

	var epoch int64

	return row.Scan(&epoch)
}

func listing(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM leases`) // want `QueryContext executes SQL from Go`
	if err != nil {
		return err
	}

	return rows.Close()
}

func preparing(ctx context.Context, db *sql.DB) error {
	stmt, err := db.PrepareContext(ctx, `SELECT 1`) // want `PrepareContext executes SQL from Go`
	if err != nil {
		return err
	}

	return stmt.Close()
}

// THE NON-CONTEXT FORMS COUNT TOO. They are the same door with a different
// signature, and a package reaching for them is exactly the case a name-only
// rule would have to enumerate.
func withoutAContext(db *sql.DB) error {
	if _, err := db.Exec(`DELETE FROM leases`); err != nil { // want `Exec executes SQL from Go`
		return err
	}

	rows, err := db.Query(`SELECT id FROM leases`) // want `Query executes SQL from Go`
	if err != nil {
		return err
	}

	return rows.Close()
}

// A HAND-ROLLED INTERFACE IS NOT AN ESCAPE HATCH, which is why the rule matches
// the SIGNATURE rather than the receiver type. This is the shape of both
// state.Querier and sqlc's own DBTX.
type reader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func throughAnInterface(ctx context.Context, q reader) error {
	rows, err := q.QueryContext(ctx, `SELECT id FROM leases`) // want `QueryContext executes SQL from Go`
	if err != nil {
		return err
	}

	return rows.Close()
}

// A STATEMENT ASSEMBLED AT RUN TIME IS STILL A STATEMENT. The rule is about the
// call, not about whether the text happens to be a literal here -- a query built
// from a variable is the harder case to review, not the easier one.
func dynamic(ctx context.Context, tx *sql.Tx, query string) error {
	_, err := tx.ExecContext(ctx, query) // want `ExecContext executes SQL from Go`

	return err
}

// A METHOD VALUE IS THE SAME DOOR THROUGH TWO NODES. Inspecting calls alone would
// have missed both: the assignment's Fun is not a selector at all, and the later
// call's Fun is an identifier. Nothing in billet takes one of these as a value, so
// reporting the selector wherever it appears closes the dodge for free.
func asAValue(ctx context.Context, db *sql.DB) error {
	exec := db.ExecContext // want `ExecContext executes SQL from Go`

	_, err := exec(ctx, `DELETE FROM leases`)

	return err
}

// A METHOD EXPRESSION IS A METHOD VALUE WITH THE RECEIVER SPELLED OUT, and the
// identifier traversal covers it for the same reason.
func asAMethodExpression(ctx context.Context, db *sql.DB) error {
	exec := (*sql.DB).ExecContext // want `ExecContext executes SQL from Go`

	_, err := exec(db, ctx, `DELETE FROM leases`)

	return err
}

// AND THE DECLARATIONS THEMSELVES ARE NOT USES. The function above is declared
// with the very name and signature the rule matches, and an interface listing one
// is the shape of state.Querier and sqlc's DBTX -- reporting either would make the
// gate refuse the adapters written to satisfy it.
type declaresOne interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type implementsOne struct{}

func (implementsOne) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return nil, nil
}

// ---- and the shapes it must stay quiet about ----

// A PACKAGE-LEVEL FUNCTION IS NOT REPORTED, EVEN WITH database/sql's EXACT SHAPE.
// Dropping the receiver requirement to catch one was tried and reverted: a LOCAL
// such function must reach database/sql in its own body, which is reported
// anyway, so the only case it uniquely catches is an imported function named
// exactly one of the eight -- while a stand-in like this one, which executes
// nothing, would be refused. See the note on executesSQL.
func ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return nil, nil
}

func throughAFunction(ctx context.Context) error {
	_, err := ExecContext(ctx, `DELETE FROM leases`)

	return err
}

// A REASONED SUPPRESSION IS ACCEPTED. This is how the allowlisted statements in
// internal/state are exempt.
func suppressed(ctx context.Context, db *sql.DB) error {
	var mode string

	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	return db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode)
}

// AN UNUSED DIRECTIVE IS REPORTED, because one that covers no finding is a
// standing exemption for whatever lands on that line next -- so this analyzer has
// to call ReportUnused and not merely Skip.
//
// THE EXPECTATION LIVES INSIDE THE DIRECTIVE because the diagnostic is reported
// at the directive's own line and nothing else can sit there. A BARE directive
// cannot be expressed this way at all, since anything trailing it becomes its
// reason; that half is covered by TestABareDirectiveIsReported in the suppress
// package, which is where the rule lives.
func staleDirective() int {
	//billet:ignore rawsql // want `suppresses nothing here`
	return 1
}

// url.Values HAS A Query METHOD AND SEVERAL PACKAGES IN THIS REPOSITORY CALL IT.
// It takes no statement and returns none of database/sql's types, which is what
// the signature test is for.
func notSQL(u *url.URL) string {
	return u.Query().Get("owner")
}

// AND NEITHER IS A METHOD THAT MERELY TAKES A STRING. A search API with a Query
// method is the false positive that would get this gate deleted.
type index struct{}

func (index) Query(ctx context.Context, term string) ([]string, error) {
	return []string{term}, nil
}

func searching(ctx context.Context, i index) error {
	_, err := i.Query(ctx, "anything")

	return err
}
