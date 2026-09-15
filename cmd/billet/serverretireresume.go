package main

import (
	"context"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/retirement"
)

// A RETIREMENT PAST THE ARCHIVE IS RESUMED FROM THE JOURNAL ALONE, because the
// host no longer holds what the request read: the identity directory has moved
// to the archive, and a server-only host's installed configuration is gone or
// about to be. Everything this run needs was recorded before the move — the
// archive's path, the staged bytes and their digest, and the locator the tail
// opens the ledger through — and nothing here reads a `server:` block, which is
// the whole point of the phase it is past.
//
// NO LEDGER IS OPENED TO REACH IT. The row was advanced to `intent` before the
// archive was, so there is nothing a ledger must be asked before `done`; and a
// host past the archive is exactly the one whose database may be out of reach,
// which the tail already answers as a PENDING ROW rather than a failed
// converge. Requiring a ledger read to resume would make an outage the one
// thing that leaves a half-transitioned host with no way forward.
//
// NO IDENTITY EXCLUSION IS TAKEN EITHER. The authority moved with the
// directory, and every action left — the rewrite, the node's restart, `done`
// and the tail — touches none of it. The archive step is the one that needs the
// exclusion, and it takes it for itself.
func retireResumeArchived(ctx context.Context, m retireMode, root *txLock, dir *os.File, shape claimShape,
	j retirement.Journal,
) (any, *retireRefusal) {
	// THE IDENTITY IS READ WHERE THE JOURNAL PUT IT. It is what says this
	// journal describes THIS deployment's retirement, and at the configured
	// path there is nothing left to read.
	identity, r := retireArchivedIdentity(j)
	if r != nil {
		return nil, r
	}

	if r := requireRetireMarker(shape, j); r != nil {
		return nil, r
	}

	// AND THE JOURNAL IS THIS HOST'S AND THIS GUARD'S. Every clause of the
	// ordinary resume's validation holds here but the row's, which is not asked
	// for the reason above.
	if err := j.Validate(retirement.JournalExpectation{Retiring: m.retiringHost, Identity: identity, Holder: m.run,
		TakenOverFrom: shape.Guard.TakenOverFrom}); err != nil {
		return nil, atRetirePhase(j, retireUnknown(retireReasonJournal, err.Error(),
			"the runbook in docs/operating/upgrades.md"))
	}

	if r := admitRetireRemaining(ctx, m, j); r != nil {
		return nil, atRetirePhase(j, r)
	}

	if j.Ownership.Owner != m.run {
		j.Rebind(m.run)

		noteRetireMutation("journal", retirement.JournalPath())
		if err := j.Write(retireNow()); err != nil {
			return nil, atRetirePhase(j, retirePersistenceError(retireReasonJournal, "record this converge as the journal's owner: ", err))
		}
	}

	// THE DRIVER TAKES NO CONFIGURATION OBSERVATION, because there may be none
	// to take. The rewrite installs the bytes the stage holds and holds them to
	// the digest intent recorded, and the observation it would otherwise update
	// belongs to a request this run is not making.
	return runRetireTransition(ctx, m, root, dir, nil, j)
}

// requireRetireMarker holds a resume to the marker on this converge's guard.
//
// THE MARKER IS REQUIRED, NOT MERELY CONSISTENT: it is what keeps the guard
// from being released under a transition that has stopped a server and moved an
// identity, and a resume without one is a host whose guard somebody could take
// away mid-transition.
func requireRetireMarker(shape claimShape, j retirement.Journal) *retireRefusal {
	switch {
	case shape.Guard.Transition == nil:
		return atRetirePhase(j, retireUnknown(retireReasonMarker, fmt.Sprintf("the retirement at %s carries no marker on "+
			"this converge's guard, so the guard could be released under it", j.Phase),
			"the runbook in docs/operating/upgrades.md"))
	case shape.Guard.Transition.ID != j.Provenance.TransitionID:
		return atRetirePhase(j, retireUnknown(retireReasonMarker, fmt.Sprintf("the guard's marker names transition %s "+
			"and the journal names %s; the marker is kept", shape.Guard.Transition.ID, j.Provenance.TransitionID), ""))
	}

	return nil
}

// retireResumeIsPastTheArchive says which of the two resume paths an INCOMPLETE
// journal takes: the ordinary one, which reads the identity and the ledger from
// the installed configuration, or the one above, which reads them from the
// journal.
//
// THE PHASE IS NOT THE WHOLE ANSWER. A journal at `stopped` whose move already
// completed is the remainder of an interruption between the rename and the
// phase's write, and that host is past the archive whatever its journal says:
// the exclusion and the identity the ordinary path would read are inside the
// directory that has moved. The filesystem is asked, and a read that cannot
// tell refuses rather than choosing a path.
func retireResumeIsPastTheArchive(j retirement.Journal, fact retirement.JournalFact) (bool, *retireRefusal) {
	if fact != retirement.JournalFactIncomplete {
		return false, nil
	}

	if j.Phase != retirement.PhaseStopped {
		return true, nil
	}

	moved, err := retireDirPresent(j.Archive)
	if err != nil {
		return false, atRetirePhase(j, retireUnknown(retireReasonIdentity, err.Error(), ""))
	}

	return moved, nil
}
