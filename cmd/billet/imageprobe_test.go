package main

import (
	"strings"
	"testing"
)

// A VERIFICATION'S MICROVM IS NOT THE NODE'S TO SWEEP.
//
// The probe's lease is invented rather than allocated, so the node daemon's sweep —
// which lists every instance this deployment owns and destroys any whose lease it
// cannot account for — is entirely correct to call it an orphan and kill it. It does
// that on a five-minute tick, against a boot-to-report window of about a minute, so
// it lands inside the window often enough that the weekly gate would periodically
// report that a perfectly good image does not work.
//
// Ownership is what keeps them apart, and it is invisible at the call site: the
// identity is one string argument that looks incidental.
func TestAProbeIsNotOwnedByTheNodeThatWouldSweepIt(t *testing.T) {
	t.Parallel()

	const deployment = "3f7c1b9a"

	got := probeDeployment(deployment)

	if got == deployment {
		t.Fatal("a verification's microVM carries the node's own identity, so the node's sweep " +
			"will find a lease it cannot account for and destroy the probe mid-verification — " +
			"reporting a healthy image as broken")
	}

	// AND IT STILL NAMES THE DEPLOYMENT IT BELONGS TO. An identity unrelated to this
	// installation would make a leftover probe unattributable on a host that runs
	// more than one billet, which is the property the deployment id exists for.
	if !strings.Contains(got, deployment) {
		t.Errorf("the probe identity %q does not carry the deployment it came from, so a "+
			"leftover would belong to nobody", got)
	}
}
