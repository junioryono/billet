package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

// registerWireNodes puts hosts in the ledger the way a registration does, and
// returns the config `billet status` will read them back through.
func registerWireNodes(t *testing.T, regs ...alloc.NodeRegistration) string {
	t.Helper()

	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	a, closeDB, err := controlPlaneAllocator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("open the ledger: %v", err)
	}

	for i := range regs {
		if _, err := a.RegisterNode(t.Context(), regs[i]); err != nil {
			t.Fatalf("register %s: %v", regs[i].Name, err)
		}
	}

	closeDB()

	return cfg
}

func wireNode(name string, negotiated int, release string) alloc.NodeRegistration {
	return alloc.NodeRegistration{
		Name:        name,
		Provider:    config.ProviderDocker,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Release:     release,
		WireMin:     nodeapi.MinVersion,
		WireMax:     negotiated,
		WireVersion: negotiated,
	}
}

// `billet status` NAMES THE HOSTS HOLDING THE OLD PROTOCOL OPEN.
//
// Driven through cmdStatus rather than through printWireWindow, because a
// correct printer says nothing about whether status calls it — the whole block
// could go from cmdStatus and a printer test would stay green. This is the
// question a server-first rollout leaves an operator with: the control plane
// speaks a range, and somebody has to be able to see which machines have not
// converged onto the top of it yet.
func TestStatusNamesTheHostsStillOnAnOlderProtocol(t *testing.T) {
	cfg := registerWireNodes(t,
		wireNode("converged", nodeapi.Version, "v9.9.9"),
		wireNode("laggard", nodeapi.MinVersion, ""),
		// A ROW WRITTEN BEFORE ANY OF THIS WAS RECORDED, which is every host in
		// the ledger until it next reconnects. It says nothing about what that
		// machine speaks, and both wrong answers are available: calling it old
		// asserts what billet does not know, and calling it converged retires a
		// protocol on the strength of a blank column.
		alloc.NodeRegistration{
			Name: "unrecorded", Provider: config.ProviderDocker,
			VCPU: 8, Memory: 32 * config.GiB,
		},
	)

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	for _, want := range []string{
		"this control plane speaks " + nodeapi.Self().String(),
		"laggard",
		"OLDER THAN THIS CONTROL PLANE",
		"NOT RECORDED, SO IT STILL BLOCKS RETIREMENT",
		// THE WHOLE NEGATIVE PHRASE. Asserting "may be dropped" was the weak
		// version: that substring is present in the REVERSED sentence too ("the
		// old protocols may be dropped"), so the assertion could not fail for the
		// one reversal it exists to catch.
		"nothing below it may be dropped",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status does not report %q:\n%s", want, out)
		}
	}

	// AND THE ALL-CLEAR IS ABSENT. Two sentences that both mention dropping is
	// how a reversal reads as a pass; this is the half that cannot be satisfied
	// by the wrong one.
	if strings.Contains(out, "free to drop") {
		t.Errorf("status says the old protocols are free to drop while hosts are still "+
			"holding them open:\n%s", out)
	}

	// AND THE UNRECORDED HOST IS NOT CALLED OLD. It might be on the newest
	// protocol; billet has not been told. Both hold retirement back, and only one
	// of them is a fact.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "unrecorded") &&
			strings.Contains(line, "OLDER THAN THIS CONTROL PLANE") {
			t.Errorf("a host billet has no protocol for is reported as older than this "+
				"control plane, which is a claim the ledger does not support:\n%s", line)
		}
	}

	// THE CONVERGED HOST MUST NOT BE FLAGGED. Without this the block passes
	// against a printer that warns about every host, which reports a rollout that
	// can never finish.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "converged") &&
			strings.Contains(line, "OLDER THAN THIS CONTROL PLANE") {
			t.Errorf("a host already on %d is reported as holding the fleet back:\n%s",
				nodeapi.Version, line)
		}
	}
}

// AND IT SAYS WHEN THE WINDOW IS CLOSED, because that is the moment a later
// release may stop carrying the old protocol — and the only safe evidence for it
// is that every recorded host has converged.
func TestStatusSaysWhenEveryHostHasConverged(t *testing.T) {
	cfg := registerWireNodes(t,
		wireNode("a", nodeapi.Version, "v9.9.9"),
		wireNode("b", nodeapi.Version, "v9.9.9"),
	)

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	if !strings.Contains(out, "free to drop in a later release") {
		t.Errorf("status does not say the window is closed on a converged fleet:\n%s", out)
	}

	if strings.Contains(out, "OLDER THAN THIS CONTROL PLANE") {
		t.Errorf("a converged fleet is reported as holding the old protocol open:\n%s", out)
	}
}

// STATUS NEVER CLAIMS AN AGE IT CANNOT KNOW.
//
// Three of the four states are not "older". A row this binary did not write says
// nothing about that host's protocol, and a row written by a NEWER binary — an
// operator rolled the control plane back — names a version this build cannot
// serve. Calling either one old is a claim the ledger does not support, and the
// unrecorded row is worse than merely wrong: the line beside it correctly calls
// it unrecorded, so the two halves would contradict each other.
func TestStatusDoesNotCallUnknownOrNewerHostsOlder(t *testing.T) {
	cfg := registerWireNodes(t,
		alloc.NodeRegistration{
			Name: "unrecorded", Provider: config.ProviderDocker,
			VCPU: 8, Memory: 32 * config.GiB,
		},
		wireNode("ahead", nodeapi.Version+1, "v9.9.9"),
	)

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	var unrecorded, ahead string

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "unrecorded "):
			unrecorded = line
		case strings.Contains(line, "ahead"):
			ahead = line
		}
	}

	for _, tc := range []struct{ what, line string }{
		{"a host billet has no protocol for", unrecorded},
		{"a host on a protocol newer than this build", ahead},
	} {
		if strings.Contains(tc.line, "OLDER THAN THIS CONTROL PLANE") {
			t.Errorf("%s is reported as older, which the ledger does not say:\n%s",
				tc.what, tc.line)
		}
	}

	// AND NEITHER IS QUIETLY TREATED AS CONVERGED, which is the other way to be
	// wrong about them and the one that retires a protocol too early.
	if strings.Contains(out, "free to drop") {
		t.Errorf("status called the fleet converged while two hosts are unaccounted "+
			"for:\n%s", out)
	}

	if !strings.Contains(unrecorded, "release unrecorded") {
		t.Errorf("the unrecorded host's release carries an age claim:\n%s", unrecorded)
	}

	if !strings.Contains(ahead, "NEWER THAN THIS CONTROL PLANE") {
		t.Errorf("a host on a protocol this build cannot serve is not reported as "+
			"such:\n%s", ahead)
	}

	// AND THE REMEDY POINTS THE RIGHT WAY. A blanket "upgrade them" is backwards
	// for a fleet that is AHEAD of its control plane — that is what a rollback
	// leaves behind, and the half that is behind is the control plane. The
	// summary used to contradict the row it was summarising.
	if !strings.Contains(out, "Upgrade or restore the control plane") {
		t.Errorf("status does not tell the operator to fix the control plane for hosts "+
			"that are ahead of it:\n%s", out)
	}

	if strings.Contains(out, "upgrade those hosts") {
		t.Errorf("status tells the operator to upgrade hosts when none of them is behind:"+
			"\n%s", out)
	}
}

// THE TWO SILENCES ARE DIFFERENT FACTS, and the report is where they are told
// apart.
//
// A host below the version that introduced the field has no release to give —
// that is the whole installed fleet, and calling it a problem would bury the
// report in noise. A host at or above it OWES the field, so its silence is a
// build that will not say what it is, which an operator planning an upgrade has
// to know before they start.
func TestStatusTellsAnOldBuildFromOneThatOwesItsRelease(t *testing.T) {
	cfg := registerWireNodes(t,
		wireNode("ancient", nodeapi.VersionNodeRelease-1, ""),
		wireNode("silent", nodeapi.VersionNodeRelease, ""),
	)

	out := capture(t, func() {
		if err := cmdStatus(t.Context(), []string{"--config", cfg}); err != nil {
			t.Errorf("status: %v", err)
		}
	})

	var ancient, silent string

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "ancient"):
			ancient = line
		case strings.Contains(line, "silent"):
			silent = line
		}
	}

	if !strings.Contains(ancient, "release unknown") {
		t.Errorf("a host older than protocol %d is not reported as simply having no release "+
			"to give:\n%s", nodeapi.VersionNodeRelease, ancient)
	}

	if !strings.Contains(silent, "NAMED NO RELEASE") {
		t.Errorf("a host on protocol %d that named no release is not reported as owing "+
			"one:\n%s", nodeapi.VersionNodeRelease, silent)
	}
}
