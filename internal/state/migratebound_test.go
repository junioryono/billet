package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// A MIGRATION BOUNDS ITSELF WHEN ITS CALLER DOES NOT, AND BEFORE IT BEGINS.
//
// The open path hands migrate a context carrying the startup budget. The
// promotion path hands it the server's, which ends at shutdown and never
// before — and a migration's statements are allowed to wait for their locks,
// so a standby promoting onto a table another session holds would sit inside
// the database for as long as the process lived, saying nothing. The bound
// cannot be exercised from a unit test without waiting the budget out, so the
// step is asserted structurally: migrate derives startupTimeout, and does so
// before the transaction begins.
func TestAMigrationBoundsItselfWhenItsCallerDoesNot(t *testing.T) {
	fset := token.NewFileSet()

	f, err := parser.ParseFile(fset, "state.go", nil, 0)
	if err != nil {
		t.Fatalf("parse state.go: %v", err)
	}

	var migrate *ast.FuncDecl

	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == "migrate" {
			migrate = fd
		}
	}

	if migrate == nil {
		t.Fatal("state.go no longer declares (*DB).migrate")
	}

	var bound, begin token.Pos

	ast.Inspect(migrate.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		x, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}

		switch {
		case x.Name == "context" && sel.Sel.Name == "WithTimeout" && len(call.Args) == 2:
			if id, ok := call.Args[1].(*ast.Ident); ok && id.Name == "startupTimeout" && !bound.IsValid() {
				bound = call.Pos()
			}
		case x.Name == "db" && sel.Sel.Name == "Tx" && !begin.IsValid():
			begin = call.Pos()
		}

		return true
	})

	if !bound.IsValid() {
		t.Fatal("migrate never derives startupTimeout for a caller whose context has no " +
			"deadline, so a promotion's migration can wait on a lock for as long as the " +
			"process lives")
	}

	if !begin.IsValid() {
		t.Fatal("migrate no longer runs through db.Tx")
	}

	if bound > begin {
		t.Errorf("the budget is derived at %s, after the transaction begins at %s; a bound "+
			"that arrives after the wait it bounds is no bound",
			fset.Position(bound), fset.Position(begin))
	}
}

// A MIGRATION STOPPED BY ITS CALLER'S DEADLINE NAMES THE BUDGET AND THE ENGINE'S
// ADVICE, ON SQLITE TOO.
//
// The PostgreSQL half of this is measured against a real server in
// postgresbackend_test.go. This one drives the same wrapping through a driver
// whose BEGIN blocks until the context ends, which is what a disk that does not
// answer looks like from here, and checks that the advice is SQLite's own rather
// than a sentence about another session — there is no other session on a file.
func TestAMigrationStoppedByItsDeadlineSaysSoOnSQLite(t *testing.T) {
	blocked := sql.OpenDB(blockingConnector{})

	t.Cleanup(func() { _ = blocked.Close() })

	db := &DB{w: blocked, backend: newSQLiteBackend(t.TempDir())}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := db.migrate(ctx)
	if err == nil {
		t.Fatal("a migration whose BEGIN never returned reported success")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the deadline's identity must survive the wrapping; got: %v", err)
	}

	for _, want := range []string{"startup budget", "disk that did not answer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}

	if strings.Contains(err.Error(), "pg_stat_activity") {
		t.Errorf("a SQLite ledger was given PostgreSQL's advice: %v", err)
	}
}

// blockingConnector hands out connections whose BeginTx waits for the context
// to end and then reports its error, the way a driver stuck on storage does.
type blockingConnector struct{}

func (blockingConnector) Connect(context.Context) (driver.Conn, error) {
	return blockingConn{}, nil
}

func (blockingConnector) Driver() driver.Driver { return blockingDriver{} }

type blockingDriver struct{}

func (blockingDriver) Open(string) (driver.Conn, error) { return nil, errStorageFault }

type blockingConn struct{}

func (blockingConn) Prepare(string) (driver.Stmt, error) { return nil, errStorageFault }
func (blockingConn) Close() error                        { return nil }
func (blockingConn) Begin() (driver.Tx, error)           { return nil, errStorageFault }

func (blockingConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	<-ctx.Done()

	return nil, ctx.Err()
}
