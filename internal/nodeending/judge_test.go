package nodeending

import (
	"errors"
	"strings"
	"testing"
)

var (
	cleanUnit   = Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "0"}
	timedOut    = Unit{ActiveState: "failed", SubState: "failed", Result: "timeout", MainPID: "0"}
	resetFailed = cleanUnit
)

func ended(invocation string, ending Ending, drainRequested bool) Entry {
	e := nodeEntry(invocation, 9)
	e.Ending, e.Source, e.DrainRequested = ending, SourceNode, drainRequested

	return e
}

func rec(entries ...Entry) Record { return Record{Schema: Schema, Entries: entries} }

// A STOP IS PROVED BY THE STOPPED INVOCATION'S OWN ENDING, never by systemd's
// exit alone: a drain passes; an idle stop and a handoff pass only where no
// drain was required; a killed process (its entry still running) fails even
// after reset-failed made systemd's triple clean; a process remaining, or an
// answer missing, is not proof; and a node that kept no record falls back to
// systemd's clean triple, labelled as the weaker proof.
func TestAStopIsProvedByItsOwnEnding(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rec           Record
		readErr       error
		invocation    string
		after         Unit
		drainRequired bool
		want          Verdict
		proof, why    string
	}{
		{"a drain", rec(ended("i1", Drained, true)), nil, "i1", cleanUnit, true, Proved, ProofRecord, "drained"},
		{"an idle stop, no drain required", rec(ended("i1", Idle, false)), nil, "i1", cleanUnit, false, Proved,
			ProofRecord, "ended idle"},
		{"an idle stop where a drain was required", rec(ended("i1", Idle, false)), nil, "i1", cleanUnit, true,
			Unproved, "", "required a drain"},
		{"a handoff, no drain required", rec(ended("i1", HandedOff, false)), nil, "i1", cleanUnit, false, Proved,
			ProofRecord, "handed-off"},
		{"a handoff the process was asked to drain", rec(ended("i1", HandedOff, true)), nil, "i1", cleanUnit, false,
			Unproved, "", "required a drain"},
		{"a timeout: still running", rec(ended("i1", Running, false)), nil, "i1", timedOut, false, Unproved, "",
			"killed or crashed"},
		{"a timeout after reset-failed", rec(ended("i1", Running, false)), nil, "i1", resetFailed, false, Unproved,
			"", "killed or crashed"},
		{"a drain cut short", rec(ended("i1", LeftRunning, true)), nil, "i1", cleanUnit, false, Unproved, "",
			"left-running"},
		{"superseded", rec(ended("i1", Superseded, false)), nil, "i1", cleanUnit, false, Unproved, "", "superseded"},
		{"another invocation's drain", rec(ended("i0", Drained, false), ended("i1", Running, false)), nil, "i1",
			cleanUnit, false, Unproved, "", "killed or crashed"},
		{"no record, systemd clean", rec(), nil, "i1", cleanUnit, true, Proved, ProofSystemd, "the floor"},
		{"no record, systemd timeout", rec(), nil, "i1", timedOut, false, Unproved, "", "failed/failed/timeout"},
		{"a process remains", rec(ended("i1", Drained, false)), nil, "i1",
			Unit{ActiveState: "deactivating", MainPID: "77"}, false, Unproved, "", "deactivating"},
		{"a MainPID remains", rec(ended("i1", Drained, false)), nil, "i1",
			Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "77"}, false, Unproved, "",
			"MainPID 77"},
		{"a job pending", rec(ended("i1", Drained, false)), nil, "i1",
			Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "0", Job: "start"}, false,
			Unproved, "", "pending start job"},
		{"ActiveState unanswered", rec(ended("i1", Drained, false)), nil, "i1", Unit{MainPID: "0"}, false, Unknown,
			"", "did not answer ActiveState"},
		{"MainPID unanswered", rec(ended("i1", Drained, false)), nil, "i1", Unit{ActiveState: "inactive"}, false,
			Unknown, "", "did not answer MainPID"},
		{"the record unreadable", rec(), errors.New("bad"), "i1", cleanUnit, false, Unknown, "",
			"could not be read"},
		{"no invocation", rec(ended("", Drained, false)), nil, "", cleanUnit, false, Unknown, "",
			"invocation is not known"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.rec, tc.readErr, tc.invocation, tc.after, tc.drainRequired)
			if got.Verdict != tc.want || got.Proof != tc.proof || !strings.Contains(got.Why, tc.why) ||
				got.Answer != tc.want.String() {
				t.Fatalf("Judge = %s proof %q (%s), want %s proof %q saying %q", got.Verdict, got.Proof, got.Why,
					tc.want, tc.proof, tc.why)
			}
		})
	}
}

// A PROBE'S ENTRY IS NEVER READ AS THE NODE'S: a probe drained under the same
// invocation proves nothing about the node's stop.
func TestAProbeEntryProvesNothingAboutTheNode(t *testing.T) {
	probe := ended("i1", Drained, false)
	probe.Role = RoleProbe

	got := Judge(rec(ended("i1", Running, false), probe), nil, "i1", cleanUnit, false)
	if got.Verdict != Unproved {
		t.Fatalf("a probe's drain proved the node's stop: %s (%s)", got.Verdict, got.Why)
	}
}

// A START IS ADMITTED BY THE NEWEST NODE ENDING: a clean ending, a systemd
// clean stop of a process that kept no record, an acknowledgement, or a reboot
// since a host-run node; a killed process, a cut-short drain or a supersession
// is refused naming the acknowledgement; with no node entry, systemd's account
// of the unit decides; and a probe never counts as the newest.
func TestAStartIsAdmittedByTheNewestEnding(t *testing.T) {
	acked := ended("i1", Running, false)
	acked.Acknowledged = true

	systemdStop := Entry{InvocationID: "i1", Role: RoleNode, Source: SourceSystemd, Ending: Stopped}

	otherBoot := ended("i1", Running, false)
	otherBoot.BootID = "boot-old"

	remoteOtherBoot := otherBoot
	remoteOtherBoot.RunsOnHost = false

	probe := ended("p1", Running, false)
	probe.Role = RoleProbe

	for _, tc := range []struct {
		name    string
		rec     Record
		readErr error
		now     Unit
		boot    string
		want    Verdict
		proof   string
		why     string
	}{
		{"a drain", rec(ended("i1", Drained, false)), nil, cleanUnit, "boot-a", Proved, ProofRecord, "drained"},
		{"a handoff", rec(ended("i1", HandedOff, false)), nil, cleanUnit, "boot-a", Proved, ProofRecord,
			"handed-off"},
		{"killed", rec(ended("i1", Running, false)), nil, timedOut, "boot-a", Unproved, "",
			`acknowledge --invocation "i1"`},
		{"killed, then reset-failed", rec(ended("i1", Running, false)), nil, resetFailed, "boot-a", Unproved, "",
			"ended running"},
		{"cut short", rec(ended("i1", LeftRunning, false)), nil, cleanUnit, "boot-a", Unproved, "",
			"left-running"},
		{"acknowledged", rec(acked), nil, timedOut, "boot-a", Proved, ProofAcknowledged, "acknowledged"},
		{"systemd's clean stop recorded", rec(ended("i0", Running, false), systemdStop), nil, cleanUnit, "boot-a",
			Proved, ProofSystemd, "kept no record"},
		{"a reboot since a host-run node", rec(otherBoot), nil, cleanUnit, "boot-a", Proved, ProofReboot,
			"earlier boot"},
		{"a reboot since a remote node", rec(remoteOtherBoot), nil, cleanUnit, "boot-a", Unproved, "",
			"ended running"},
		{"the boot unknown", rec(otherBoot), nil, cleanUnit, "", Unproved, "", "ended running"},
		{"a probe is not the newest", rec(ended("i1", Running, false), probe), nil, cleanUnit, "boot-a", Unproved,
			"", `"i1"`},
		{"no record, systemd clean", rec(), nil, cleanUnit, "boot-a", Proved, ProofSystemd, "no ending"},
		{"no record, systemd timeout", rec(), nil, timedOut, "boot-a", Unproved, "", "--unrecorded"},
		{"a process running", rec(ended("i1", Drained, false)), nil,
			Unit{ActiveState: "active", SubState: "running", Result: "success", MainPID: "5"}, "boot-a", Unproved,
			"", "is active"},
		{"unreadable", rec(), errors.New("bad"), cleanUnit, "boot-a", Unknown, "", "could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AdmitStart(tc.rec, tc.readErr, tc.now, tc.boot)
			if got.Verdict != tc.want || got.Proof != tc.proof || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("AdmitStart = %s proof %q (%s), want %s proof %q saying %q", got.Verdict, got.Proof,
					got.Why, tc.want, tc.proof, tc.why)
			}
		})
	}
}
