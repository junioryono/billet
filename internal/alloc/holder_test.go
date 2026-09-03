package alloc

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// THE HOLDER IS THE PROCESS THAT WAS GIVEN THE WORK, NAMED BY ITS INCARNATION,
// and a report can tell it from the process the host runs now.
//
// INCARNATION, NOT EPOCH, and the second registration below is the whole reason.
// The registration epoch moves on every registration, and the same process
// registers again whenever a control plane restarts or forgets it — so a report
// built on the epoch would call every surviving lease's holder replaced after an
// ordinary restart. The incarnation is minted once per process and changes when
// the process does.
func TestALeaseRecordsTheProcessThatHoldsIt(t *testing.T) {
	now := time.Now().UTC()
	tiers := []config.Tier{tier("billet-4vcpu-a", 4, 8*config.GiB)}
	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, tiers,
		WithClock(func() time.Time { return now }))

	lease := reserve(t, a, tiers[0].Label)

	// The host the tier pins to, registered as the backend the lease accepts, by
	// the process that will launch the compute.
	host := NodeRegistration{
		Name: lease.TargetNode, Provider: lease.Providers[0], VCPU: 8, Memory: 32 * config.GiB,
		Incarnation: "process-one",
	}

	first, err := a.RegisterNode(t.Context(), host)
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
		t.Fatalf("bind: %v", err)
	}

	bound, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read bound lease: %v", err)
	}
	if bound.HolderIncarnation != "process-one" {
		t.Fatalf("bound lease records holder %q, want the binding process", bound.HolderIncarnation)
	}

	for _, phase := range []Phase{PhaseAssigned, PhaseLaunching, PhaseOnline, PhaseBusy} {
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
			t.Fatalf("advance to %s: %v", phase, err)
		}
	}

	replaced := func(what string) []ReplacedHolderLease {
		t.Helper()
		out, err := a.RunningWithReplacedHolder(t.Context())
		if err != nil {
			t.Fatalf("RunningWithReplacedHolder %s: %v", what, err)
		}

		return out
	}

	if got := replaced("under the launching process"); len(got) != 0 {
		t.Fatalf("a lease held by the current process was reported as replaced: %+v", got)
	}

	// THE SAME PROCESS REGISTERS AGAIN — a control plane restarted and forgot it.
	// The epoch moves; the holder is exactly who it was.
	second, err := a.RegisterNode(t.Context(), host)
	if err != nil {
		t.Fatalf("re-register the same process: %v", err)
	}
	if second <= first {
		t.Fatalf("re-registration epoch %d did not move past %d", second, first)
	}
	if got := replaced("after the same process re-registered"); len(got) != 0 {
		t.Fatalf("an ordinary re-registration was reported as a replaced holder: %+v", got)
	}

	// A DIFFERENT PROCESS REGISTERS UNDER THE HOST'S NAME. The process that was
	// given the work is no longer the one the deployment talks to, and the
	// running lease says so.
	host.Incarnation = "process-two"
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("register a replacement process: %v", err)
	}

	got := replaced("after a replacement process registered")
	if len(got) != 1 || got[0].ID != lease.ID {
		t.Fatalf("running leases with a replaced holder = %+v, want lease %s", got, lease.ID)
	}
	if h := got[0].Holder; h.Incarnation != "process-one" || h.NodeIncarnation != "process-two" ||
		!h.Replaced() || !h.NodeLive {
		t.Fatalf("holder = %+v, want process-one on a live host now run by process-two", h)
	}

	// ENTERING TEARDOWN RE-RECORDS THE HOLDER AS THE PROCESS TAKING IT — the one
	// registered now — and the held report names it as current again.
	if err := a.Advance(t.Context(), lease.ID, lease.Epoch, PhaseTeardown); err != nil {
		t.Fatalf("advance to teardown: %v", err)
	}
	held, err := a.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if len(held) != 1 || held[0].ID != lease.ID {
		t.Fatalf("held = %+v, want lease %s", held, lease.ID)
	}
	if h := held[0].Holder; h.Incarnation != "process-two" || h.Replaced() {
		t.Fatalf("teardown holder = %+v, want process-two and not replaced", h)
	}

	// A THIRD PROCESS REPLACES THAT HOLDER TOO, which the held report shows
	// beside the phase.
	host.Incarnation = "process-three"
	if _, err := a.RegisterNode(t.Context(), host); err != nil {
		t.Fatalf("register a third process: %v", err)
	}
	held, err = a.Held(t.Context())
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if h := held[0].Holder; !h.Replaced() || h.NodeIncarnation != "process-three" ||
		h.Incarnation != "process-two" {
		t.Fatalf("held holder = %+v, want process-two replaced by process-three", h)
	}

	// AND AN EMPTY SIDE IS UNKNOWN, NEVER REPLACED: a row an older binary wrote
	// records no holder, a loopback host presents no incarnation, and "cannot
	// tell" must not read as "the holder is gone".
	if (Holder{Incarnation: "", NodeIncarnation: "process-three", NodeKnown: true}).Replaced() {
		t.Fatal("an unrecorded holder reads as replaced")
	}
	if (Holder{Incarnation: "process-one", NodeIncarnation: "", NodeKnown: true}).Replaced() {
		t.Fatal("a holder on a host that presented no incarnation reads as replaced")
	}
	if (Holder{Incarnation: "process-one", NodeIncarnation: "process-two", NodeKnown: false}).Replaced() {
		t.Fatal("a holder on an unregistered host reads as replaced")
	}
}
