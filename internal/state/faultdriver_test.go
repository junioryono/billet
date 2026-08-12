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
// and callers test for context.Canceled. The obvious way to do that — ask the
// context first and return its error — quietly discarded SQLITE_CORRUPT and
// SQLITE_IOERR whenever cancellation raced the return, which is the one class of
// error that must never be hidden behind a routine cancellation.
//
// Both identities, therefore, and this is what pins it.
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

	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancellation must keep its identity, got: %v", err)
	}

	if !errors.Is(err, errStorageFault) {
		t.Errorf("the storage fault must survive alongside it, got: %v", err)
	}
}
