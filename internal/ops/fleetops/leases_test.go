package fleetops

import (
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// THE HELD TABLE NAMES THE PROCESS HOLDING EACH LEASE, AND SAYS WHEN IT WAS
// REPLACED — with "unknown" for a lease nothing recorded a holder for, because
// "cannot tell" rendered as the first process would send an operator to force a
// lease whose holder may be perfectly alive.
func TestHeldLeasesNameTheirHolderAndWhetherItWasReplaced(t *testing.T) {
	held := []alloc.HeldLease{
		{
			ID: "aaaa", Tier: "billet-2vcpu", Node: "epyc-1", State: alloc.PhaseTeardown,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{
				Incarnation: "3333333333333333aaaa", NodeIncarnation: "5555555555555555bbbb",
				NodeKnown: true, NodeLive: true,
			},
		},
		{
			ID: "bbbb", Tier: "billet-2vcpu", Node: "epyc-1", State: alloc.PhaseCustody,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{
				Incarnation: "5555555555555555bbbb", NodeIncarnation: "5555555555555555bbbb",
				NodeKnown: true, NodeLive: true,
			},
		},
		{
			ID: "cccc", Tier: "billet-2vcpu", Node: "old-host", State: alloc.PhaseQuarantine,
			VCPU: 2, Memory: 8 * config.GiB,
			Holder: alloc.Holder{},
		},
	}

	out := capture(t, func() {
		PrintHeld(processEnv(), held)
		PrintHolderNote(os.Stdout, held)
	})

	for _, want := range []string{
		"HOLDER",
		"process 333333333333, REPLACED by 555555555555",
		"unknown",
		"A holder marked REPLACED",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the held table does not say %q:\n%s", want, out)
		}
	}

	// A CURRENT HOLDER IS NAMED AND NOT CALLED REPLACED.
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "bbbb") {
			continue
		}
		if !strings.Contains(line, "process 555555555555") || strings.Contains(line, "REPLACED") {
			t.Errorf("a current holder is misreported: %s", line)
		}
	}

	if strings.Contains(out, "process ,") || strings.Contains(out, "process \t") {
		t.Errorf("an unrecorded holder was rendered as a process:\n%s", out)
	}

	// THE NOTE IS PRINTED ONLY WHEN SOMETHING WAS REPLACED. A table with live
	// holders alone must not carry a paragraph about dead ones.
	quiet := capture(t, func() { PrintHolderNote(os.Stdout, held[1:]) })
	if quiet != "" {
		t.Errorf("the replaced-holder note was printed with nothing replaced:\n%s", quiet)
	}
}
