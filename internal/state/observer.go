package state

import "time"

// Observer is told how the ledger's write transactions went: how long each
// waited for the writer slot, how long it held it and whether it committed,
// and each time a busy BEGIN was retried. It is what shows the single writer
// slot filling up before scheduling stalls behind it.
//
// CALLED ON THE WRITER'S OWN GOROUTINE: a retry while the write waits, before
// any transaction is open, and the wait and the hold together once the
// transaction has ended, each measured where it ended so the observer's own
// time is in neither. An implementation must still return at once and touch
// nothing that could reach the ledger. The hold ends when Tx's own commit or
// rollback returns; a rollback database/sql runs by itself after the context
// is cancelled can still be finishing then, so a hold cut short by
// cancellation is the lower bound of the true one.
type Observer interface {
	WriteWaited(d time.Duration)
	WriteHeld(d time.Duration, committed bool)
	WriteRetried()
}

// Observe sets what this handle tells about its writes; nil, the default,
// tells nothing. It may be called while transactions run.
func (db *DB) Observe(o Observer) {
	if o == nil {
		db.observer.Store(nil)

		return
	}

	db.observer.Store(&o)
}

// observing is the observer in force, or nil.
func (db *DB) observing() Observer {
	if p := db.observer.Load(); p != nil {
		return *p
	}

	return nil
}
