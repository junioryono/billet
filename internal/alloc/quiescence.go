package alloc

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/state"
)

// Outstanding is one lease standing between a deployment and quiescence.
type Outstanding struct {
	ID    string
	Tier  string
	Node  string
	Phase Phase
	// RunID is GitHub's workflow run, where the lease carries one. It is what
	// makes a report about work recognisable to the person whose work it is.
	RunID string
	Since string
	// Deregistered says whether GitHub's runner registration has been removed.
	// A lease that still carries one is work GitHub can route to; one without is
	// compute billet is still destroying.
	Deregistered bool
}

// Quiescence is what a deployment is still holding.
//
// WHAT IT COUNTS. A lease implies compute, or a registration GitHub can route
// to, or capacity a listener can still turn into either — and it implies none of
// those once it reaches a terminal phase. Everything short of terminal counts.
//
// ESCROWED CAPACITY COUNTS, and an earlier version of this did not. The
// reasoning for excluding it was that such a lease holds no compute and carries
// no runner, which is true and is not the question: while the listener is alive
// it still ADVERTISES that lease, so GitHub can assign against it and the lease
// becomes running work with nothing having changed on the host and admission
// never having reopened. A barrier that samples before that transition reports
// quiet about a deployment that is about to start a job. Counting it is
// conservative in the only direction that is safe — a drain that waits too long
// costs patience, one that stops too early costs somebody's build.
//
// It is REPORTED separately, because the two are different waits. Running work
// ends by finishing. Escrow ends when the listener releases it, which is what a
// sealed listener must do — until it does, a drain on a busy deployment waits on
// something no amount of patience resolves, and that is a gap in the listener
// rather than in this query.
//
// WHAT IT CANNOT SEE, stated rather than claimed away: compute whose lease has
// already gone. A listener that loses a running lease keeps an in-memory
// obligation to destroy what it launched, and a launch whose lease was reclaimed
// can create compute it then fails to destroy. Neither is in the ledger, and
// neither survives a restart of the process holding it. The ordinary cases are
// covered — restart-adopted compute keeps its launching/online/busy/custody
// lease, and an out-of-contact holder becomes quarantine, which is counted — but
// a barrier built on the ledger cannot answer for compute the ledger never
// hears about again.
//
// WHAT ANSWERS IT IS ComputeClearance, AND THIS TYPE IS DELIBERATELY UNCHANGED.
// That is a SECOND barrier — it asks each host what its provider is actually
// running and records the answer against a fence taken before the question —
// and folding it in here would silently change what every existing caller of
// Quiet() is told. The two are also ORDERED rather than independent: the
// compute barrier is meaningful only once this one holds nothing, because while
// a lease is open a launch may legitimately be dispatched and would discard
// whatever a host had proved.
type Quiescence struct {
	// Sealed says whether the deployment is refusing new work. A deployment that
	// is quiet but open is not quiesced: the next poll may fill it.
	Sealed bool
	// Generation is the admission generation this snapshot saw.
	//
	// A WAITER NEEDS IT, and Sealed alone is not enough: between two samples
	// somebody can resume and seal again, and a waiter watching only the boolean
	// sees "sealed" both times and never learns that admission was open in
	// between — during which the deployment could have taken work. Comparing the
	// generation against the one the drain established turns that into something
	// observable.
	Generation int64
	// Outstanding is every lease short of terminal, oldest first.
	Outstanding []Outstanding
}

// Quiet reports whether a drain may stop waiting.
func (q Quiescence) Quiet() bool { return q.Sealed && len(q.Outstanding) == 0 }

// RegistrationUnconfirmed counts outstanding leases whose GitHub registration
// has not been confirmed removed.
//
// NAMED FOR WHAT THE FLAG PROVES, which is less than "GitHub can route here":
// `deregistered` records that a RemoveRunner call succeeded, so an unset flag
// covers a lease that never registered at all — an assigned lease, an early
// launch, an ambiguous pre-registration custody. Calling that "routable" would
// tell an operator something stronger than billet knows.
func (q Quiescence) RegistrationUnconfirmed() int {
	var n int

	for _, o := range q.Outstanding {
		if !o.Deregistered {
			n++
		}
	}

	return n
}

// Escrowed counts outstanding leases that hold no compute yet — capacity a
// listener is advertising, or has promised to GitHub and cannot withdraw.
func (q Quiescence) Escrowed() int {
	var n int

	for _, o := range q.Outstanding {
		if o.Phase == PhaseCapacity {
			n++
		}
	}

	return n
}

// quiescencePhases are the phases that imply compute exists or GitHub can route
// to it. Written out rather than expressed as "not terminal and not capacity",
// so adding a phase is a decision about draining rather than a silent inclusion.
var quiescencePhases = []Phase{
	PhaseCapacity,
	PhaseAssigned, PhaseLaunching, PhaseOnline, PhaseBusy,
	PhaseCustody, PhaseTeardown, PhaseQuarantine,
}

// Quiescence reports what the deployment is still holding, and whether new work
// can still arrive.
//
// ON THE READ-ONLY POOL, because a drain asks this on a cadence while the
// control plane is doing real work, and a question must not reserve the single
// writer slot to answer itself.
func (a *Allocator) Quiescence(ctx context.Context) (Quiescence, error) {
	var q Quiescence

	// ONE SNAPSHOT FOR BOTH HALVES. Read separately, a resume committing in
	// between yields "sealed, and holding nothing" about a deployment that is
	// open — a composite that was never true at any instant, and the one an
	// operator would act on.
	err := a.db.View(ctx, func(tx querier) error {
		admission, err := state.ReadAdmission(ctx, tx)
		if err != nil {
			return fmt.Errorf("alloc: read admission for quiescence: %w", err)
		}

		// UNKNOWN COUNTS AS SEALED here for the same reason it does at admission:
		// a state billet could not read is not one that said work may arrive.
		q.Sealed = admission.Sealed()
		q.Generation = admission.Generation

		rows, err := state.ReadQueries(tx).ListOutstandingLeases(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list outstanding leases: %w", err)
		}

		for _, row := range rows {
			q.Outstanding = append(q.Outstanding, Outstanding{
				ID:           row.ID,
				Tier:         row.Tier,
				Node:         row.Node,
				Phase:        Phase(row.Phase),
				RunID:        row.RunID,
				Since:        row.Since,
				Deregistered: row.Deregistered == 1,
			})
		}

		return nil
	})
	if err != nil {
		return Quiescence{}, err
	}

	return q, nil
}
