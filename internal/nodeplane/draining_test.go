package nodeplane

import (
	"context"
	"sync"
	"testing"
)

// drainingLedger records every draining mark the plane asks for.
type drainingLedger struct {
	countingRegistrar

	mu     sync.Mutex
	marked []string
	epochs []int64
}

func (d *drainingLedger) NodeDraining(_ context.Context, _ string, epoch int64, incarnation string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.marked = append(d.marked, incarnation)
	d.epochs = append(d.epochs, epoch)

	return nil
}

// THE MARK IS FENCED TO THE PROCESS THAT REFUSED. A replacement that registers
// under the name after the refusal was delivered, and before it is recorded, must
// not be the one marked: it would stay out of placement for as long as it ran
// without registering again. The window sits between a result reaching the
// waiting Launch and markDraining, which no wire test can stage, so the plane is
// driven at it directly.
func TestADrainingMarkIsFencedToTheProcessThatRefused(t *testing.T) {
	t.Parallel()

	ledger := &drainingLedger{}
	p := testPlane(t, WithRegistrar(ledger))

	registerAs(t, p, "refused")

	p.mu.Lock()
	n := p.nodes["n1"]
	pend := &pending{incarnation: n.incarnation, takenLedgerEpoch: n.ledgerEpoch}
	p.mu.Unlock()

	if pend.takenLedgerEpoch == 0 {
		t.Fatal("the registration recorded no ledger epoch, so this proves nothing")
	}

	registerAs(t, p, "replacement")

	p.markDraining(t.Context(), "n1", pend)

	ledger.mu.Lock()
	defer ledger.mu.Unlock()

	if len(ledger.marked) != 1 || ledger.marked[0] != "refused" ||
		ledger.epochs[0] != pend.takenLedgerEpoch {
		t.Errorf("marked %v at epochs %v, want the refusing process at epoch %d, not its replacement",
			ledger.marked, ledger.epochs, pend.takenLedgerEpoch)
	}
}
