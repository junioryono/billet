package nodeplane

import (
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A LATE FAILED LAUNCH ENDS ITS OWN LEASE'S OWNERSHIP AND NO OTHER: a later launch of the
// same request is another lease, whose compute may be running.
func TestALateFailedLaunchForgetsOnlyItsOwnLease(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	p.owners = map[string]leaseOwner{
		"timed-out": {node: "n1", incarnation: "p1", requestID: 7, seq: 1},
		"later":     {node: "n1", incarnation: "p1", requestID: 7, seq: 2},
	}

	p.mu.Lock()
	err := p.settleAbandonedLocked("n1", abandonedCmd{
		kind: nodeapi.CommandLaunch, incarnation: "p1", requestID: 7, leaseID: "timed-out",
	}, nodeapi.CommandResult{ID: "c1", Error: "the launch failed"})
	p.mu.Unlock()

	if err != nil {
		t.Fatalf("settle a late failed launch: %v", err)
	}
	if _, kept := p.owners["timed-out"]; kept {
		t.Error("the failed launch's own lease is still owned")
	}
	if _, kept := p.owners["later"]; !kept {
		t.Error("a later launch of the same request lost its ownership to an earlier one's failure")
	}

	// A LEASE SINCE HANDED TO THE NODE'S NEXT PROCESS (#374) is that process's record.
	p.owners["handed"] = leaseOwner{node: "n1", incarnation: "p2", requestID: 8, seq: 3}

	p.mu.Lock()
	err = p.settleAbandonedLocked("n1", abandonedCmd{
		kind: nodeapi.CommandLaunch, incarnation: "p1", requestID: 8, leaseID: "handed",
	}, nodeapi.CommandResult{ID: "c2", Error: "the launch failed"})
	p.mu.Unlock()

	if err != nil {
		t.Fatalf("settle a late failed launch of a lease handed over: %v", err)
	}
	if _, kept := p.owners["handed"]; !kept {
		t.Error("a late failure from the process that gave a lease up ended its next process's ownership")
	}
}

// A DESTROY'S PROOF ENDS ONLY THE OWNERSHIP IT COULD HAVE TORN DOWN: records made by
// launches delivered by the time it was taken, and those no delivery made. A launch
// of the same request given after it ran after it, on compute it never touched, and
// forgetting that record would leave its compute owned by no process.
func TestADestroyForgetsOnlyTheOwnershipDeliveredBeforeIt(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	p.owners = map[string]leaseOwner{
		"adopted": {node: "n1", incarnation: "p1", requestID: 7},
		"before":  {node: "n1", incarnation: "p1", requestID: 7, seq: 1},
		"after":   {node: "n1", incarnation: "p1", requestID: 7, seq: 3},
		"other":   {node: "n1", incarnation: "p1", requestID: 8, seq: 1},
	}

	p.forgetForRequest(7, 2)

	for id, want := range map[string]bool{"adopted": false, "before": false, "after": true, "other": true} {
		if _, kept := p.owners[id]; kept != want {
			t.Errorf("record %q kept %v, want %v", id, kept, want)
		}
	}
}

// A DESTROY ASKED OF EVERY NODE ENDS, ON EACH, ONLY WHAT WAS DELIVERED THERE BY THE TIME
// THAT NODE TOOK IT: each took it at its own moment, and a launch delivered to one node
// after its leg was taken ran after it there.
func TestADestroyAskedOfEveryNodeForgetsEachNodesOwnBefore(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	p.owners = map[string]leaseOwner{
		"adopted":  {node: "n3", incarnation: "p3", requestID: 7},
		"a-before": {node: "n1", incarnation: "p1", requestID: 7, seq: 2},
		"a-after":  {node: "n1", incarnation: "p1", requestID: 7, seq: 4},
		"b-before": {node: "n2", incarnation: "p2", requestID: 7, seq: 4},
		"silent":   {node: "n3", incarnation: "p3", requestID: 7, seq: 1},
	}

	p.forgetForRequestOn(7, map[string]uint64{"n1": 3, "n2": 5})

	for id, want := range map[string]bool{
		"adopted": false, "a-before": false, "a-after": true, "b-before": false, "silent": true,
	} {
		if _, kept := p.owners[id]; kept != want {
			t.Errorf("record %q kept %v, want %v", id, kept, want)
		}
	}
}

// THE FAILED LEASE'S OWN RECORD DECIDES, NOT WHICHEVER RECORD OF ITS REQUEST COMES FIRST:
// two leases of one request held by two processes. Asked over fresh planes, since which
// record a map yields first is the map's choice.
func TestALateFailedLaunchReadsItsOwnLeasesRecord(t *testing.T) {
	t.Parallel()

	for range 64 {
		p := testPlane(t)
		p.owners = map[string]leaseOwner{
			"mine":   {node: "n1", incarnation: "p1", requestID: 9, seq: 1},
			"theirs": {node: "n1", incarnation: "p2", requestID: 9, seq: 2},
			"gone":   {node: "n1", incarnation: "p2", requestID: 10, seq: 3},
			"kept":   {node: "n1", incarnation: "p1", requestID: 10, seq: 4},
		}

		p.mu.Lock()
		for _, entry := range []abandonedCmd{
			{kind: nodeapi.CommandLaunch, incarnation: "p1", requestID: 9, leaseID: "mine"},
			{kind: nodeapi.CommandLaunch, incarnation: "p1", requestID: 10, leaseID: "gone"},
		} {
			if err := p.settleAbandonedLocked("n1", entry, nodeapi.CommandResult{Error: "failed"}); err != nil {
				p.mu.Unlock()
				t.Fatalf("settle %s: %v", entry.leaseID, err)
			}
		}
		p.mu.Unlock()

		for id, want := range map[string]bool{"mine": false, "theirs": true, "gone": true, "kept": true} {
			if _, kept := p.owners[id]; kept != want {
				t.Fatalf("record %q kept %v, want %v", id, kept, want)
			}
		}
	}
}
