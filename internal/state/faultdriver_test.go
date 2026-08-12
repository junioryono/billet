package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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

	// Only the writer is exercised; Tx touches nothing else on the handle.
	db := &DB{w: faulty}

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

// A CANCELLED QUERY MUST COME BACK AS SOMETHING beginWrite CAN CLASSIFY.
//
// beginWrite translates exactly one driver code into the caller's context error,
// and 9 was a guess until this measured it. This project's rule is that a claim
// about somebody else's code is pinned to what that code does, not to what its
// documentation implies.
//
// MEASURED, AND ONLY ABOUT THE QUERY PATH: modernc normalises a cancelled QUERY
// to context.Canceled itself. That is not evidence about BeginTx, which is what
// beginWrite uses and which need not behave the same way — so this says nothing
// about whether the SQLITE_INTERRUPT branch is reachable, and an earlier version
// of this comment wrongly implied it did. The branch's own behaviour is pinned
// separately, by TestAnInterruptedBeginIsReportedAsCancellation.
//
// What this one is worth: if a future driver version stops normalising the query
// path, that is a signal the transaction path may have changed too.
//
// Either answer passes. Anything else fails, because beginWrite would hand a
// caller an error it cannot recognise as cancellation at all.
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

	err := db.w.QueryRowContext(ctx,
		`WITH RECURSIVE counter(x) AS (
		     SELECT 1 UNION ALL SELECT x + 1 FROM counter WHERE x < 200000000
		 ) SELECT count(*) FROM counter`).Scan(&n)
	if err == nil {
		t.Fatal("the query was expected to be interrupted; if this machine is fast enough " +
			"to finish 200M rows in 50ms the row count needs raising")
	}

	if errors.Is(err, context.Canceled) || isInterrupt(err) {
		return
	}

	t.Errorf("a cancelled query produced %#v, which is neither a context error nor "+
		"recognised by isInterrupt — beginWrite would return it to a caller testing "+
		"for context.Canceled", err)
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

	db := &DB{w: faulty}

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
}
