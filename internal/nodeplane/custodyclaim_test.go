package nodeplane

import (
	"errors"
	"slices"
	"testing"
)

// A CONTROLLER THAT REPLACED ANOTHER KNOWS NOTHING OF WHO HOLDS WHAT (#685), so a
// superseded process says, and the claim takes only what no process was given: an
// adoption from the ledger by the current process gives way to the process that holds
// the compute, whether or not the current one can see it too, and a delivery never
// does.
func TestACustodyClaimTakesOnlyWhatNoOtherProcessWasGiven(t *testing.T) {
	t.Parallel()

	const (
		lease   = "l9"
		request = int64(7)
	)

	claimant := leaseOwner{node: "n1", incarnation: "p1", requestID: request}

	seed := func(p *Plane, owner leaseOwner) {
		p.mu.Lock()
		defer p.mu.Unlock()

		if p.owners == nil {
			p.owners = make(map[string]leaseOwner)
		}

		p.owners[lease] = owner
	}

	for name, tc := range map[string]struct {
		setup func(t *testing.T, p *Plane)
		held  bool
		added bool
		owner leaseOwner
	}{
		"no owner": {
			setup: func(*testing.T, *Plane) {},
			held:  true, added: true, owner: claimant,
		},
		"delivered to the claimant": {
			setup: func(_ *testing.T, p *Plane) { seed(p, claimant) },
			held:  true, owner: claimant,
		},
		// A PROCESS THAT REACHED THE NEW CONTROLLER BEFORE IT LEARNED IT WAS SUPERSEDED
		// adopted its own lease, with no request; a destroy then cannot find it.
		"the claimant's by its own registration's adoption": {
			setup: func(_ *testing.T, p *Plane) { seed(p, leaseOwner{node: "n1", incarnation: "p1"}) },
			held:  true, owner: claimant,
		},
		// THE ONE THE ISSUE IS ABOUT: the registration filled the gap from the ledger,
		// and the process it filled it for has no such compute.
		"adopted by the current process, whose report leaves it out": {
			setup: func(_ *testing.T, p *Plane) {
				p.AdoptOwnershipWithInventory("n1", "p2", nil, true)
				p.AdoptOwnership("n1", "p2", []string{lease})
			},
			held: true, added: true, owner: claimant,
		},
		// A REPORT IS NOT AN ADOPTION: the current process may have seen a guest it has
		// not taken on, so the process that holds the compute goes on holding it, as it
		// did on the plane before.
		"adopted by the current process, whose report names it": {
			setup: func(_ *testing.T, p *Plane) {
				p.AdoptOwnershipWithInventory("n1", "p2", []string{lease}, true)
			},
			held: true, added: true, owner: claimant,
		},
		"adopted by the current process, whose report is unknown": {
			setup: func(_ *testing.T, p *Plane) { p.AdoptOwnership("n1", "p2", []string{lease}) },
			held:  true, added: true, owner: claimant,
		},
		"delivered to the current process": {
			setup: func(_ *testing.T, p *Plane) {
				seed(p, leaseOwner{node: "n1", incarnation: "p2", requestID: request})
			},
			owner: leaseOwner{node: "n1", incarnation: "p2", requestID: request},
		},
		"delivered to a process that is no longer current": {
			setup: func(_ *testing.T, p *Plane) {
				seed(p, leaseOwner{node: "n1", incarnation: "p0", requestID: request})
			},
			owner: leaseOwner{node: "n1", incarnation: "p0", requestID: request},
		},
		// TWO SUPERSEDED PROCESSES DO NOT TAKE IT IN TURNS, refusing each the other's
		// renewal on every claim.
		"adopted by a process that is no longer current": {
			setup: func(_ *testing.T, p *Plane) { seed(p, leaseOwner{node: "n1", incarnation: "p0"}) },
			owner: leaseOwner{node: "n1", incarnation: "p0"},
		},
		"another host's": {
			setup: func(_ *testing.T, p *Plane) { seed(p, leaseOwner{node: "n2", incarnation: "p9"}) },
			owner: leaseOwner{node: "n2", incarnation: "p9"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := testPlane(t, WithRegistrar(newLedger()))
			registerAs(t, p, "p1")
			registerAs(t, p, "p2")
			tc.setup(t, p)

			held, added := p.ClaimCustody("n1", "p1", []CustodyClaim{{LeaseID: lease, RequestID: request}})

			if got := slices.Contains(held, lease); got != tc.held {
				t.Errorf("held = %v, want the lease held: %v", held, tc.held)
			}

			if got := slices.Contains(added, lease); got != tc.added {
				t.Errorf("added = %v, want the lease added: %v", added, tc.added)
			}

			if owner, ok := ownerOf(p, lease); !ok || owner != tc.owner {
				t.Errorf("the owner is %+v (ok=%v), want %+v", owner, ok, tc.owner)
			}

			// WHAT THE ANSWER SAYS IS WHAT THE WIRE THEN DOES: a process told it
			// holds a lease may renew it, and one not told so is refused.
			byOwner, err := p.AuthorizeLease("n1", "p1", lease)
			if tc.held && (err != nil || !byOwner) {
				t.Errorf("a process that holds the lease is refused it: byOwner=%v err=%v", byOwner, err)
			}

			if !tc.held && !errors.Is(err, ErrSuperseded) {
				t.Errorf("a process that does not hold the lease is admitted: byOwner=%v err=%v", byOwner, err)
			}

			// AND THE CURRENT PROCESS IS NOT LOCKED OUT: it may hold the compute too.
			if tc.owner.node == "n1" {
				if _, err := p.AuthorizeLease("n1", "p2", lease); err != nil {
					t.Errorf("the current process is refused the lease: %v", err)
				}
			}
		})
	}
}

// THE REQUEST COMES WITH THE LEASE, so a completion's destroy finds the process that
// holds the compute. Without it the plane's only other answer is to broadcast to the
// current process, which truthfully has nothing to remove, and the lease is released
// under the running job.
func TestAClaimedLeaseAnswersForItsRequest(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	registerAs(t, p, "p2")

	if _, known := p.OwnerOfRequest(7); known {
		t.Fatal("a plane that launched nothing knows the request's owner")
	}

	p.ClaimCustody("n1", "p1", []CustodyClaim{{LeaseID: "l9", RequestID: 7}})

	owner, known := p.OwnerOfRequest(7)
	if !known || owner.Incarnation != "p1" || owner.Current {
		t.Fatalf("the request's owner is %+v (known=%v), want p1, not current", owner, known)
	}
}

// A REGISTRATION ONLY FILLS GAPS, so a claim made before the current process reaches
// this controller stays with the claimant, as it does after.
func TestAClaimSurvivesTheRegistrationThatFollowsIt(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))

	p.ClaimCustody("n1", "p1", []CustodyClaim{{LeaseID: "l9", RequestID: 7}})

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, true)
	p.AdoptOwnership("n1", "p2", []string{"l9"})

	if owner, ok := ownerOf(p, "l9"); !ok || owner.incarnation != "p1" || owner.requestID != 7 {
		t.Fatalf("the owner is %+v (ok=%v), want the claimant", owner, ok)
	}
}
