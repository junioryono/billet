package state

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
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
