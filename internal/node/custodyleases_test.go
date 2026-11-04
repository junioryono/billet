package node

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// WHAT A SUPERSEDED PROCESS SAYS IT HOLDS IS EVERYTHING IT HOLDS COMPUTE FOR, each once:
// a controller that replaced the one it knew refuses it whatever it leaves out. A job
// in custody, a job a launch left running after the supersession, and a launch still
// under way, which this process renews until it ends.
func TestAProcessNamesEveryLeaseItHoldsComputeFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, r *Runner, a *leaseMaker)
	}{
		{"in custody", func(t *testing.T, r *Runner, a *leaseMaker) {
			t.Helper()
			a.launch(t, r)
			r.Superseded()

			if !r.HoldsInCustodyForTest(a.leaseID) {
				t.Fatal("the job is not in custody")
			}
		}},
		{"running", func(t *testing.T, r *Runner, a *leaseMaker) {
			t.Helper()
			a.launch(t, r)
		}},
		{"launching", func(t *testing.T, r *Runner, a *leaseMaker) {
			t.Helper()
			t.Cleanup(r.holdWhileLaunching(a.lease(), "billet-launching"))
		}},
		{"in custody and launching, named once", func(t *testing.T, r *Runner, a *leaseMaker) {
			t.Helper()
			a.launch(t, r)
			r.Superseded()
			t.Cleanup(r.holdWhileLaunching(a.lease(), "billet-launching"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := newLeaseMaker(t)
			r := a.runner

			if got := r.CustodyLeases(); len(got) != 0 {
				t.Fatalf("a process holding nothing names %v", got)
			}

			tc.stage(t, r, a)

			if got := r.CustodyLeases(); !slices.Equal(got, []string{a.leaseID}) {
				t.Fatalf("the process names %v, want %s once", got, a.leaseID)
			}
		})
	}
}

// leaseMaker is one runner over a fake backend and one lease assigned to it.
type leaseMaker struct {
	runner  *Runner
	leaseID string
	lease   func() *alloc.Lease
}

func newLeaseMaker(t *testing.T) *leaseMaker {
	t.Helper()

	a, host := newAllocatorWithHost(t)
	lease := assignedLease(t, a)

	return &leaseMaker{
		runner:  New(a, host, &fakeJIT{setID: 7}, &fakeProvider{kind: config.ProviderDocker}, nil),
		leaseID: lease.ID,
		lease:   func() *alloc.Lease { return lease },
	}
}

func (m *leaseMaker) launch(t *testing.T, r *Runner) {
	t.Helper()

	if err := r.Launch(t.Context(), m.lease(), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
}
