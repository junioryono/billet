package alloc

import (
	"errors"
	"testing"
	"time"
)

// A LEASE A NODE HAS TAKEN CUSTODY OF IS NOT RELEASED UNDERNEATH IT.
//
// Custody means the node asked its backend to stop the guest and did not get
// proof it stopped, so it is holding the lease until the compute is provably
// gone. Terminalising it here would free a slot whose container may still be on
// the host, seconds after the node took responsibility for exactly that question
// — the overcommit the whole ordering exists to prevent. `billet leases release
// --force` is the operation for that case, because it goes THROUGH the holder
// rather than changing the ledger underneath it.
func TestForceTerminateRefusesALeaseWithALiveHolder(t *testing.T) {
	// QUARANTINE IS IN THIS TABLE BECAUSE LEAVING IT OUT WAS A P0.
	//
	// The check began as a DENYLIST of custody and teardown, on the reasoning that
	// it is only reached after a confirmed destroy. That reasoning was wrong twice:
	// destroyAll reports a CUSTODY answer as "done" — deliberately, because the
	// node has taken responsibility — so the caller's map is not proof of absence
	// at all; and the reaper can move a lease into quarantine between the destroy
	// and this transaction, which the denylist permitted. Either way capacity was
	// freed for compute nobody proved gone.
	for _, phase := range []Phase{PhaseCustody, PhaseTeardown, PhaseQuarantine} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)

			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			err := a.ForceTerminate(t.Context(), lease.ID)
			if !errors.Is(err, ErrForceHeld) {
				t.Fatalf("ForceTerminate on a %s lease: %v, want ErrForceHeld", phase, err)
			}

			held, err := a.Lease(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("Lease: %v", err)
			}

			if held.Phase.Terminal() {
				t.Errorf("a %s lease was terminalised as %s underneath its holder",
					phase, held.Phase)
			}
		})
	}
}

// A FORCED LEASE IS ARCHIVED AS FAILED, NEVER AS DONE.
//
// A job whose runner was destroyed while it was executing did not finish, and
// recording `done` puts a lie in the history of somebody's build — the same
// objection the launch path makes about a runner that never ran.
func TestForceTerminateArchivesAsFailedWithAReason(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.ForceTerminate(t.Context(), lease.ID); err != nil {
		t.Fatalf("ForceTerminate: %v", err)
	}

	// READ FROM THE HISTORY, because Lease refuses a terminal row: a lease that
	// released its capacity is deliberately not readable as an open one.
	outcome, err := a.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryOutcome: %v", err)
	}

	if outcome != string(PhaseFailed) {
		t.Errorf("a forced lease is archived as %s, want failed", outcome)
	}

	// THE HISTORY SAYS WHY, so a forced failure is distinguishable from an
	// ordinary one when somebody asks what happened to their build.
	reason, err := a.HistoryFailureReason(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryFailureReason: %v", err)
	}

	if reason != forceDestroyFailureReason {
		t.Errorf("the forced lease records reason %q, want %q", reason,
			forceDestroyFailureReason)
	}
}

// AN EARLIER FAILURE REASON SURVIVES. The fact that arrived first is the cause;
// the teardown is what followed from it.
func TestForceTerminateKeepsAnExistingFailureReason(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if err := a.MarkFailure(t.Context(), lease.ID, lease.Epoch,
		"the host lost its disk"); err != nil {
		t.Fatalf("MarkFailure: %v", err)
	}

	if err := a.ForceTerminate(t.Context(), lease.ID); err != nil {
		t.Fatalf("ForceTerminate: %v", err)
	}

	reason, err := a.HistoryFailureReason(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("HistoryFailureReason: %v", err)
	}

	if reason != "the host lost its disk" {
		t.Errorf("the forced lease records %q; the earlier cause must survive", reason)
	}
}

// IDEMPOTENT, because a listener that restarts re-observes the durable force
// request and may act on a target a previous incarnation already settled.
func TestForceTerminateIsIdempotent(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	for range 2 {
		if err := a.ForceTerminate(t.Context(), lease.ID); err != nil {
			t.Fatalf("ForceTerminate: %v", err)
		}
	}
}

// A LEASE THAT IS ALREADY GONE IS THE OUTCOME THIS WANTED. The reaper or a
// concurrent settlement got there first; nothing is left holding capacity, which
// is what the caller was trying to achieve.
func TestForceTerminateAcceptsAMissingLease(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	if err := a.ForceTerminate(t.Context(), "no-such-lease"); err != nil {
		t.Errorf("ForceTerminate on a lease that is gone: %v, want nil", err)
	}
}

// THE CANDIDATE LIST COVERS RUNNING COMPUTE AND NOTHING ELSE.
//
// Custody, teardown and quarantine are held by a NODE rather than by a listener,
// and billet already has the operation for them. Including them here would be a
// second mechanism for the same situation — the one that skips the handoff.
func TestForceDestroyCandidatesExcludeLeasesANodeHolds(t *testing.T) {
	for _, phase := range []Phase{PhaseCustody, PhaseTeardown} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)

			candidates, err := a.ForceDestroyCandidates(t.Context(), "", "")
			if err != nil {
				t.Fatalf("ForceDestroyCandidates: %v", err)
			}

			if len(candidates) != 1 || candidates[0].ID != lease.ID {
				t.Fatalf("a busy lease is not a candidate: %+v", candidates)
			}

			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance: %v", err)
			}

			held, err := a.ForceDestroyCandidates(t.Context(), "", "")
			if err != nil {
				t.Fatalf("ForceDestroyCandidates: %v", err)
			}

			if len(held) != 0 {
				t.Errorf("a %s lease is a force-destroy candidate: %+v; its compute is held "+
					"by a node and `billet leases release --force` is what resolves it",
					phase, held)
			}
		})
	}
}

// ESCROW IS NOT A FORCE TARGET. A lease in `capacity` holds no compute — a drain
// hands it back by itself, and destroying it would report ending a job that had
// never started.
func TestForceDestroyCandidatesExcludeEscrow(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	if _, err := a.Reserve(t.Context(), "small"); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	candidates, err := a.ForceDestroyCandidates(t.Context(), "", "")
	if err != nil {
		t.Fatalf("ForceDestroyCandidates: %v", err)
	}

	if len(candidates) != 0 {
		t.Errorf("escrowed capacity is a force-destroy candidate: %+v", candidates)
	}
}

// FILTERING BY HOST FINDS A LEASE THAT IS ONLY AIMED AT IT.
//
// `assigned` is in the force-destroy phase list precisely because such a lease is
// already in the listener's running map and nothing else will return it -- but a
// lease is not BOUND to a host until it launches, so until then only target_node
// names the machine. A `--node` filter reading `node` alone therefore skips
// exactly the leases the phase list was widened to reach, and an operator
// clearing one host would leave them stranded.
//
// COALESCE(node, target_node) IS THE RULE EVERYWHERE ELSE IN THIS PACKAGE, for
// the same reason: capacity is charged the moment a reservation names a machine.
// Measured: narrowing this one filter to `node` left every other test in this
// package green.
func TestForceDestroyCandidatesFindALeaseOnlyAimedAtAHost(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, 11); err != nil {
		t.Fatalf("Assign: %v", err)
	}

	// STILL UNBOUND: escrow chose the host, nothing has launched on it, so `node`
	// is NULL and only target_node names the machine.
	assigned, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if assigned.Node != "" {
		t.Fatalf("the lease is already bound to %q, so this test cannot observe the "+
			"unbound case it exists for", assigned.Node)
	}

	target := assigned.TargetNode
	if target == "" {
		t.Fatal("the assigned lease names no target host, so there is nothing to filter on")
	}

	candidates, err := a.ForceDestroyCandidates(t.Context(), "", target)
	if err != nil {
		t.Fatalf("ForceDestroyCandidates: %v", err)
	}

	if len(candidates) != 1 || candidates[0].ID != lease.ID {
		t.Fatalf("filtering on %s missed the lease aimed at it: %+v; an operator "+
			"clearing that host would leave it stranded", target, candidates)
	}

	if candidates[0].Node != target {
		t.Errorf("the candidate reports host %q, want %q", candidates[0].Node, target)
	}
}
