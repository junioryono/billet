package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
	"time"
)

// errStorageFault stands in for the failures that must never be swallowed —
// SQLITE_CORRUPT, SQLITE_IOERR, anything saying the disk is in trouble.
var errStorageFault = errors.New("state: simulated storage fault")

// faultConnector hands out connections whose BeginTx cancels the caller's
// context and THEN fails for an unrelated reason.
//
// THE SEAM I SAID DID NOT EXIST. An earlier round documented the context check in
// beginWrite as untestable, on the grounds that nothing could make the driver
// take its interrupt path on demand. That was wrong: the driver is an interface,
// so the case can be staged directly rather than provoked. The lesson is worth
// the file — "there is no seam" deserved one more minute of looking.
type faultConnector struct{ cancel context.CancelFunc }

func (c faultConnector) Connect(context.Context) (driver.Conn, error) {
	return faultConn(c), nil
}

func (faultConnector) Driver() driver.Driver { return faultDriver{} }

type faultDriver struct{}

func (faultDriver) Open(string) (driver.Conn, error) { return nil, errStorageFault }

type faultConn struct{ cancel context.CancelFunc }

func (faultConn) Prepare(string) (driver.Stmt, error) { return nil, errStorageFault }
func (faultConn) Close() error                        { return nil }
func (faultConn) Begin() (driver.Tx, error)           { return nil, errStorageFault }

// BeginTx cancels first and fails second, which is the interleaving that used to
// lose the failure.
func (c faultConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.cancel()

	return nil, errStorageFault
}

// A CANCELLED TRANSACTION MUST NOT SWALLOW A STORAGE FAULT.
//
// beginWrite has to report cancellation with its own identity, because modernc
// can interrupt a BEGIN and return SQLITE_INTERRUPT rather than a context error,
// and callers test for context.Canceled. Two ways of doing that were wrong.
//
// Asking the context first and returning its error discarded SQLITE_CORRUPT and
// SQLITE_IOERR whenever cancellation raced the return. Joining the two kept both
// identities structurally and still lost the fault where it counts: callers
// filter on errors.Is(err, context.Canceled) and treat a match as a clean
// shutdown, so a joined error is dropped exactly like a pure cancellation.
//
// So only a genuine interrupt is translated, and a storage fault arriving
// alongside cancellation is still reported AS the fault. This pins that.
func TestACancelledBeginStillReportsAStorageFault(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	faulty := sql.OpenDB(faultConnector{cancel: cancel})

	t.Cleanup(func() { _ = faulty.Close() })

	// Only the writer and the backend are exercised. The backend is real because
	// Tx asks it to classify the error, and this test is entirely about that
	// classification; nothing here reaches its path.
	db := &DB{w: faulty, backend: newSQLiteBackend(t.TempDir())}

	err := db.Tx(ctx, func(*sql.Tx) error {
		t.Error("the callback must not run when the transaction never began")

		return nil
	})
	if err == nil {
		t.Fatal("a failed BeginTx must be reported")
	}

	if !errors.Is(err, errStorageFault) {
		t.Errorf("the storage fault must be what is reported, got: %v", err)
	}

	// AND IT MUST NOT READ AS A CANCELLATION. This is the half that matters:
	// nodeplane's handler and the server's shutdown classifier both discard an
	// error that matches context.Canceled, so a fault wearing that identity is a
	// fault nobody ever sees.
	if errors.Is(err, context.Canceled) {
		t.Errorf("a storage fault must not be classified as a cancellation, got: %v", err)
	}
}

// A DEPENDENCY PROBE, AND NOTHING MORE THAN THAT.
//
// It records how modernc reports a cancelled QUERY today: as context.Canceled,
// normalised by the driver. It says nothing about BeginTx, which is the path
// beginWrite uses and which need not behave the same way — so it is not evidence
// about whether the SQLITE_INTERRUPT branch is reachable, and it does not pin the
// code that branch matches on. Both of those are somebody else's behaviour that
// this package cannot reach from here.
//
// The branch's own behaviour is pinned by
// TestAnInterruptedBeginIsReportedAsCancellation, which drives it end to end.
//
// What this one earns its place with: if a future driver version stops
// normalising the query path, that is the first sign the transaction path may
// have moved too — and this fails, loudly, in a file whose comment then tells the
// next reader where to look.
func TestACancelledQueryIsRecognisableAsCancellation(t *testing.T) {
	db := open(t)

	ctx, cancel := context.WithCancel(t.Context())

	// Cancelled from outside while the scan is still running, so the timing is
	// caused rather than hoped for.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	var n int

	// TWO BILLION RATHER THAN TWO HUNDRED MILLION, and the larger number is FREE.
	// The query is cancelled after 50ms either way, so nothing here ever counts to
	// the bound — it exists only to guarantee the statement is still running when
	// the cancellation lands. At 200M this was a wall-clock race against row
	// throughput on whatever machine happened to run it, with the failure being a
	// spurious "the query was expected to be interrupted" on a fast one.
	err := db.w.QueryRowContext(ctx,
		`WITH RECURSIVE counter(x) AS (
		     SELECT 1 UNION ALL SELECT x + 1 FROM counter WHERE x < 2000000000
		 ) SELECT count(*) FROM counter`).Scan(&n)
	if err == nil {
		t.Fatal("the query was expected to be interrupted; if this machine is fast enough " +
			"to finish 2B rows in 50ms the row count needs raising")
	}

	if errors.Is(err, context.Canceled) || isInterrupt(err) {
		return
	}

	t.Errorf("a cancelled query produced %#v, which is neither a context error nor "+
		"recognised by isInterrupt. This driver used to normalise it; if that has "+
		"changed, check whether BeginTx changed with it — beginWrite's interrupt "+
		"handling assumes SQLite's code 9", err)
}

// AN INTERRUPTED BEGIN IS REPORTED AS CANCELLATION.
//
// This is the branch itself, driven end to end: beginWrite receives an error its
// classifier calls an interrupt while the context is cancelled, and must hand the
// caller context.Canceled rather than the driver's error.
//
// The classifier is swapped rather than provoked because modernc's *sqlite.Error
// has unexported fields, so a code-9 error cannot be built from here. What that
// leaves unpinned is narrow and stated: whether 9 is the right code. What it pins
// is everything downstream of that decision, which is where the behaviour lives —
// removing the translation fails this by returning the driver's error instead.
func TestAnInterruptedBeginIsReportedAsCancellation(t *testing.T) {
	restore := isInterrupt
	isInterrupt = func(error) bool { return true }

	t.Cleanup(func() { isInterrupt = restore })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	faulty := sql.OpenDB(faultConnector{cancel: cancel})

	t.Cleanup(func() { _ = faulty.Close() })

	db := &DB{w: faulty, backend: newSQLiteBackend(t.TempDir())}

	err := db.Tx(ctx, func(*sql.Tx) error {
		t.Error("the callback must not run when the transaction never began")

		return nil
	})
	if err == nil {
		t.Fatal("a failed BeginTx must be reported")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupted BEGIN on a cancelled context must report cancellation, got: %v", err)
	}

	// AND THE DRIVER ERROR MUST NOT STILL BE THERE. Matching context.Canceled is
	// satisfied by errors.Join(original, ctxErr) — which is one of the two shapes
	// this rule explicitly rejects, because every filter discards a joined error
	// exactly like a pure cancellation while it still matches the fault. Asserting
	// only the positive half would bless it.
	if errors.Is(err, errStorageFault) {
		t.Errorf("the driver's error must be substituted rather than joined, got: %v", err)
	}
}

// AN INTERRUPTED READ BEGIN IS REPORTED AS CANCELLATION.
//
// THE SAME BRANCH ONE POOL OVER, AND IT WAS MISSING. `Tx` translated
// SQLITE_INTERRUPT and `View` did not, so a read interrupted by a shutdown
// reached its caller as a storage error — and every caller that filters a clean
// stop does so with errors.Is(err, context.Canceled), which a driver code never
// matches. MEASURED in CI before the fix: the control plane's startup capacity
// refresh returned "begin read tx: interrupted (9)", `onlyCancellation` correctly
// declined to call it a cancellation, and a deliberate stop exited non-zero.
//
// Staged the way the write-path test stages it, and for the same reason: modernc's
// *sqlite.Error has unexported fields, so the classifier is swapped rather than
// provoked. What that leaves unpinned is whether 9 is the right code; what it pins
// is everything downstream of the decision.
func TestAnInterruptedReadBeginIsReportedAsCancellation(t *testing.T) {
	restore := isInterrupt
	isInterrupt = func(error) bool { return true }

	t.Cleanup(func() { isInterrupt = restore })

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	faulty := sql.OpenDB(faultConnector{cancel: cancel})

	t.Cleanup(func() { _ = faulty.Close() })

	// The READER pool, because that is the one View uses.
	db := &DB{r: faulty, backend: newSQLiteBackend(t.TempDir())}

	err := db.View(ctx, func(Querier) error {
		t.Error("the callback must not run when the transaction never began")

		return nil
	})
	if err == nil {
		t.Fatal("a failed read BeginTx must be reported")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("an interrupted read BEGIN on a cancelled context must report "+
			"cancellation, got: %v", err)
	}

	// See the write-path test: a join would satisfy the assertion above.
	if errors.Is(err, errStorageFault) {
		t.Errorf("the driver's error must be substituted rather than joined, got: %v", err)
	}

	// THE ACCOUNT SURVIVES EVEN THOUGH THE IDENTITY DOES NOT. A bare ctx.Err()
	// passes both assertions above and takes with it the only record of what
	// billet was doing — which is how a cancelled migration stopped naming the
	// migration.
	if !strings.Contains(err.Error(), errStorageFault.Error()) {
		t.Errorf("the interrupted operation must still be named, got: %v", err)
	}
}

// AND A CANCELLED READ STILL REPORTS A STORAGE FAULT.
//
// The other half, for the same reason the write path has it: only a genuine
// interrupt may be translated. A fault that arrives alongside cancellation and is
// handed back wearing context.Canceled is a fault nobody ever sees, because every
// shutdown classifier discards exactly that identity.
func TestACancelledReadBeginStillReportsAStorageFault(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	faulty := sql.OpenDB(faultConnector{cancel: cancel})

	t.Cleanup(func() { _ = faulty.Close() })

	db := &DB{r: faulty, backend: newSQLiteBackend(t.TempDir())}

	err := db.View(ctx, func(Querier) error {
		t.Error("the callback must not run when the transaction never began")

		return nil
	})
	if err == nil {
		t.Fatal("a failed read BeginTx must be reported")
	}

	if !errors.Is(err, errStorageFault) {
		t.Errorf("the storage fault must be what is reported, got: %v", err)
	}

	if errors.Is(err, context.Canceled) {
		t.Errorf("a storage fault must not be classified as a cancellation, got: %v", err)
	}
}

// errFakeInterrupt stands in for a driver saying the statement was interrupted:
// SQLITE_INTERRUPT, or PostgreSQL's SQLSTATE 57014.
var errFakeInterrupt = errors.New("state: simulated driver interrupt")

// A BEGIN IS NOT THE ONLY PLACE A CANCELLATION ARRIVES.
//
// The rule was written for beginWrite and lived inside it, so it covered exactly
// one call. PostgreSQL cancels the STATEMENT rather than the transaction, so
// 57014 reaches the caller from a query INSIDE the callback — every generated
// read query in internal/state/queryset.go forwards its error verbatim — and
// nothing was translating it there at all. asCancellation now sits on every
// error leaving Tx and View.
//
// The classifier is swapped to recognise a sentinel rather than to say yes to
// everything, because this test needs a real transaction to have begun: an
// always-true classifier would translate the storage fault its sibling asserts
// on.
func TestACancelledCallbackIsReportedAsCancellation(t *testing.T) {
	restore := isInterrupt
	isInterrupt = func(err error) bool { return errors.Is(err, errFakeInterrupt) }

	t.Cleanup(func() { isInterrupt = restore })

	for _, run := range []struct {
		name string
		call func(*DB, context.Context, func() error) error
	}{
		{"View", func(db *DB, ctx context.Context, fn func() error) error {
			return db.View(ctx, func(Querier) error { return fn() })
		}},
		{"Tx", func(db *DB, ctx context.Context, fn func() error) error {
			return db.Tx(ctx, func(*sql.Tx) error { return fn() })
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			db := open(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			// CANCELLED FROM INSIDE, so the transaction has genuinely begun and the
			// error is one the callback produced — which is the shape this covers
			// and the shape the begin-time tests cannot reach.
			err := run.call(db, ctx, func() error {
				cancel()

				return fmt.Errorf("read rows: %w", errFakeInterrupt)
			})
			if err == nil {
				t.Fatal("the callback's failure must be reported")
			}

			if !errors.Is(err, context.Canceled) {
				t.Errorf("a driver interrupt on a cancelled context must report "+
					"cancellation, got: %v", err)
			}

			// See the begin-time tests: a join satisfies the assertion above.
			if errors.Is(err, errFakeInterrupt) {
				t.Errorf("the driver's error must be substituted rather than joined, got: %v", err)
			}

			if !strings.Contains(err.Error(), "read rows") {
				t.Errorf("the interrupted operation must still be named, got: %v", err)
			}
		})
	}
}

// AND A CANCELLED CALLBACK STILL REPORTS A STORAGE FAULT.
//
// The same half the begin-time pair has, one layer in. Widening the translation
// to every error leaving Tx and View is only safe while it stays keyed on the
// classifier: an unconditional substitution here would swallow SQLITE_CORRUPT
// from a scan, and every shutdown classifier discards exactly that identity.
func TestACancelledCallbackStillReportsAStorageFault(t *testing.T) {
	for _, run := range []struct {
		name string
		call func(*DB, context.Context, func() error) error
	}{
		{"View", func(db *DB, ctx context.Context, fn func() error) error {
			return db.View(ctx, func(Querier) error { return fn() })
		}},
		{"Tx", func(db *DB, ctx context.Context, fn func() error) error {
			return db.Tx(ctx, func(*sql.Tx) error { return fn() })
		}},
	} {
		t.Run(run.name, func(t *testing.T) {
			db := open(t)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			err := run.call(db, ctx, func() error {
				cancel()

				return errStorageFault
			})
			if err == nil {
				t.Fatal("the callback's failure must be reported")
			}

			if !errors.Is(err, errStorageFault) {
				t.Errorf("the storage fault must be what is reported, got: %v", err)
			}

			if errors.Is(err, context.Canceled) {
				t.Errorf("a storage fault must not be classified as a cancellation, got: %v", err)
			}
		})
	}
}

// EVERY UNTRANSACTED READ IS ACCOUNTED FOR, BECAUSE EACH ONE OWES A CALL NOTHING
// MAKES FOR IT.
//
// Tx and View put every error they return through asCancellation. DB.Reader hands
// out a Querier with no transaction around it, so a caller that uses it is
// outside both — and `ScaleSets` was: Server.Run calls it before any listener
// starts and returns what comes back, so a stop landing in that window left the
// unit `failed` over a read the shutdown had interrupted. Found by review, after
// the fix that was supposed to have covered every path.
//
// A closed set rather than a rule about what the code must say, because "the
// enclosing function also mentions asCancellation" is satisfied by a mention in a
// comment and broken by a helper one call deeper. Adding a Reader() caller is
// meant to be a decision somebody writes down here.
//
// KEYED ON THE FUNCTION, NOT THE FILE, AND THE FIRST VERSION WAS KEYED ON THE
// FILE. That version listed scaleset.go and admin.go, and the standby branch then
// added a `db.Reader()` inside openDir — in state.go, which also holds Tx, View
// and asCancellation themselves. Listing state.go would have accounted for that
// one call AND for every future untransacted read added anywhere in the file this
// rule is defined in, which is the one file where a hole is least likely to be
// noticed. The AST is parsed so each CALL SITE is its own decision.
//
// WHAT IT DOES NOT CATCH, measured rather than assumed: deleting the
// asCancellation call inside ScaleSets leaves this GREEN. The table's notes are
// prose, not assertions — this test is about the SET being accounted for, and
// nothing here re-derives whether a listed caller still does what its note says.
// Closing that needs a cancellation the driver reports as its own error from a
// real query, which database/sql does not produce: it checks the context before
// the driver is reached and returns ctx.Err() itself. So the note is a decision
// on the record and the set is the guard.
func TestEveryUntransactedReadIsAccountedFor(t *testing.T) {
	// "<file>:<function>" -> why this one does not translate, or how it does.
	known := map[string]string{
		"scaleset.go:ScaleSets": "translates: it passes its error through db.asCancellation",
		// BOTH OF THESE ARE THE OPEN PATH, AND THAT IS THE WHOLE REASON. Neither
		// runs on a handle that has been returned to anybody, so there is no
		// deliberate stop to report: a failure is a handle that never opened, and
		// a non-zero exit is the right answer for it. Only openDir reaches either.
		"admin.go:verifySchema": "does not, deliberately: the open path, reached when a " +
			"handle may not migrate and must prove the schema is already what this binary " +
			"expects",
		"state.go:openDir": "does not, deliberately: the open path, where a STANDBY proves " +
			"the schema is not AHEAD of this binary — a process that does not know every " +
			"applied version could not serve the deployment if it were promoted",
	}

	found := readerCallSites(t)

	if len(found) == 0 {
		t.Fatal("no DB.Reader() call sites were found in this package; this test would pass " +
			"against an empty table and a package that had stopped using it alike")
	}

	for site := range found {
		if _, ok := known[site]; !ok {
			t.Errorf("%s reads through DB.Reader(), which is outside Tx and View — so a "+
				"cancelled statement reaches its caller as the driver's own error unless it "+
				"calls db.asCancellation itself. Decide which it is and record it in this "+
				"test's table.", site)
		}
	}

	for site := range known {
		if !found[site] {
			t.Errorf("%s is listed here as a DB.Reader() caller and no longer is; drop the "+
				"entry so the set keeps meaning something", site)
		}
	}
}

// readerCallSites returns every "<file>:<function>" in this package that calls
// DB.Reader, excluding the declaration itself.
//
// PARSED RATHER THAN GREPPED, because the answer is which FUNCTION holds the
// call and a text scan cannot say. The first version stripped the declaration's
// source text and searched for ".Reader()" in what was left, which is both too
// coarse (one entry covers a whole file) and too fragile (it depends on the
// declaration being spelled exactly as the strip string).
//
// FILE BY FILE RATHER THAN parser.ParseDir, which is deprecated — and the
// replacement is better here rather than merely equivalent: ParseDir would not
// have associated files by build tag either, and what this needs is EVERY file
// whatever platform it compiles on. lock_unix.go is `//go:build unix`, and a
// read added there owes the same decision as one added anywhere else. That is
// the same reason `make lint` runs a second pass under GOOS=linux.
func readerCallSites(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}

	fset := token.NewFileSet()
	sites := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}

			// The method's own declaration is not a use of it.
			if fn.Name.Name == "Reader" && fn.Recv != nil {
				continue
			}

			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}

				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Reader" {
					return true
				}

				sites[name+":"+fn.Name.Name] = true

				return true
			})
		}
	}

	return sites
}
