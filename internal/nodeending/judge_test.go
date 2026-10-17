package nodeending

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

var (
	cleanUnit   = Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "0", JobAnswered: true}
	timedOut    = Unit{ActiveState: "failed", SubState: "failed", Result: "timeout", MainPID: "0", JobAnswered: true}
	resetFailed = cleanUnit
)

func ended(invocation string, ending Ending, drainRequested bool) Entry {
	e := nodeEntry(invocation, 9)
	e.Ending, e.Source, e.DrainRequested = ending, SourceNode, drainRequested

	return e
}

func rec(entries ...Entry) Record { return Record{Schema: Schema, Entries: entries} }

func trimmed(entries ...Entry) Record {
	r := rec(entries...)
	r.Trimmed = true

	return r
}

func systemdStopped(invocation string) Entry {
	return Entry{InvocationID: invocation, BootID: "boot-a", Role: RoleNode, Source: SourceSystemd, Ending: Stopped}
}

// A STOP IS PROVED BY THE STOPPED INVOCATION'S OWN ENDING, never by systemd's
// exit alone: a drain passes; a handoff passes only where no drain was
// required; an idle stop is as good as the process before it; a killed process
// (its entry still running) fails even after reset-failed made systemd's
// triple clean; an invocation a newer process followed proves nothing; a
// process remaining, or an answer missing, is not proof; and a node that kept
// no record falls back to systemd's clean triple, labelled as the weaker
// proof, only over a history that admits a start.
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
		acknowledge   string
		floor         bool
	}{
		{name: "a drain", rec: rec(ended("i1", Drained, true)), invocation: "i1", after: cleanUnit,
			drainRequired: true, want: Proved, proof: ProofRecord, why: "drained"},
		{name: "an idle first process, no drain required", rec: rec(ended("i1", Idle, false)), invocation: "i1",
			after: cleanUnit, want: Proved, proof: ProofRecord, why: "no earlier process is recorded"},
		{name: "an idle first process where a drain was required", rec: rec(ended("i1", Idle, false)),
			invocation: "i1", after: cleanUnit, drainRequired: true, want: Proved, proof: ProofRecord,
			why: "no earlier process is recorded"},
		{name: "idle over a drain, a drain required", rec: rec(ended("i0", Drained, false), ended("i1", Idle, false)),
			invocation: "i1", after: cleanUnit, drainRequired: true, want: Proved, proof: ProofRecord,
			why: `ended idle, so the host is as the process before it left it: invocation "i0"`},
		{name: "idle over a killed process", rec: rec(ended("i0", Running, false), ended("i1", Idle, false)),
			invocation: "i1", after: cleanUnit, want: Unproved, why: `invocation "i0" (pid 9, started`,
			acknowledge: "--invocation i1"},
		{name: "idle over a handoff, a drain required",
			rec: rec(ended("i0", HandedOff, false), ended("i1", Idle, false)), invocation: "i1", after: cleanUnit,
			drainRequired: true, want: Unproved, why: "never adopted it", acknowledge: "--invocation i1"},
		{name: "idle over a handoff, no drain required",
			rec: rec(ended("i0", HandedOff, false), ended("i1", Idle, false)), invocation: "i1", after: cleanUnit,
			want: Proved, proof: ProofRecord, why: "handed its compute to the next process"},
		{name: "idle asked to drain over a handoff",
			rec: rec(ended("i0", HandedOff, false), ended("i1", Idle, true)), invocation: "i1", after: cleanUnit,
			want: Unproved, why: "never adopted it", acknowledge: "--invocation i1"},
		{name: "idle over an acknowledgement, a drain required", rec: rec(acknowledged(ended("i0", Running, false)),
			ended("i1", Idle, false)), invocation: "i1", after: cleanUnit, drainRequired: true, want: Unproved,
			why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "idle with the record trimmed below it", rec: trimmed(ended("i1", Idle, false)), invocation: "i1",
			after: cleanUnit, want: Unknown, why: "trimmed", acknowledge: "--invocation i1"},
		{name: "a handoff, no drain required", rec: rec(ended("i1", HandedOff, false)), invocation: "i1",
			after: cleanUnit, want: Proved, proof: ProofRecord, why: "handed-off"},
		{name: "a handoff the process was asked to drain", rec: rec(ended("i1", HandedOff, true)), invocation: "i1",
			after: cleanUnit, want: Unproved, why: "required a drain", acknowledge: "--invocation i1"},
		{name: "a timeout: still running", rec: rec(ended("i1", Running, false)), invocation: "i1",
			after: timedOut, want: Unproved, why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "a timeout after reset-failed", rec: rec(ended("i1", Running, false)), invocation: "i1",
			after: resetFailed, want: Unproved, why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "a drain cut short", rec: rec(ended("i1", LeftRunning, true)), invocation: "i1", after: cleanUnit,
			want: Unproved, why: "left-running", acknowledge: "--invocation i1"},
		{name: "superseded", rec: rec(ended("i1", Superseded, false)), invocation: "i1", after: cleanUnit,
			want: Unproved, why: "superseded", acknowledge: "--invocation i1"},
		{name: "another invocation's drain", rec: rec(ended("i0", Drained, false), ended("i1", Running, false)),
			invocation: "i1", after: cleanUnit, want: Unproved, why: "killed or crashed",
			acknowledge: "--invocation i1"},
		{name: "a newer process after the drained one",
			rec: rec(ended("i1", Drained, false), ended("i2", LeftRunning, false)), invocation: "i1",
			after: cleanUnit, want: Unproved, why: `not the newest process: invocation "i2"`,
			acknowledge: "--invocation i2"},
		{name: "a probe after the drained one", rec: rec(ended("i1", Drained, false), probeEntry("p1")),
			invocation: "i1", after: cleanUnit, want: Proved, proof: ProofRecord, why: "drained"},
		{name: "no record, systemd clean", rec: rec(), invocation: "i1", after: cleanUnit, drainRequired: true,
			want: Proved, proof: ProofSystemd, why: "the floor", floor: true},
		{name: "no record, systemd clean over a drain", rec: rec(ended("i0", Drained, false)), invocation: "i1",
			after: cleanUnit, want: Proved, proof: ProofSystemd, why: "the floor", floor: true},
		{name: "no record, systemd clean over a killed process", rec: rec(ended("i0", Running, false)),
			invocation: "i1", after: cleanUnit, want: Unproved, why: "only over a history that passes this stop as well",
			acknowledge: "--invocation i0"},
		{name: "no record, systemd timeout", rec: rec(), invocation: "i1", after: timedOut, want: Unproved,
			why: "failed/failed/timeout", acknowledge: "--unrecorded", floor: true},
		{name: "no record, systemd clean over an acknowledgement, a drain required",
			rec: rec(acknowledged(ended("i0", Running, false))), invocation: "i1", after: cleanUnit,
			drainRequired: true, want: Unproved, why: "passes this stop as well"},
		{name: "no record, systemd clean over a handoff, a drain required", rec: rec(ended("i0", HandedOff, false)),
			invocation: "i1", after: cleanUnit, drainRequired: true, want: Unproved, why: "never adopted it",
			acknowledge: "--invocation i0"},
		{name: "no record, systemd's failure recorded before reset-failed",
			rec:        rec(Entry{InvocationID: "i1", Role: RoleNode, Source: SourceSystemd, Ending: Failed}),
			invocation: "i1", after: cleanUnit, want: Unproved, why: "when it was first judged",
			acknowledge: "--invocation i1"},
		{name: "no record, systemd's failure acknowledged, then reset-failed",
			rec: rec(acknowledged(Entry{InvocationID: "i1", Role: RoleNode, Source: SourceSystemd,
				Ending: Failed})), invocation: "i1", after: cleanUnit, want: Unproved,
			why: "when it was first judged"},
		{name: "no record, systemd's Result unanswered", rec: rec(), invocation: "i1",
			after: Unit{ActiveState: "inactive", SubState: "dead", MainPID: "0", JobAnswered: true}, want: Unknown,
			why: "inactive/dead/(unanswered)"},
		{name: "no record over an acknowledged killed process, a drain required",
			rec: rec(acknowledged(ended("i0", Running, false))), invocation: "i1", after: cleanUnit,
			drainRequired: true, want: Unproved, why: "cannot prove a drain itself"},
		{name: "a process remains", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{ActiveState: "deactivating", MainPID: "77", JobAnswered: true}, want: Unproved,
			why: "deactivating"},
		{name: "a MainPID remains", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "77",
				JobAnswered: true}, want: Unproved, why: "MainPID 77"},
		{name: "a job pending", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "0", Job: "start",
				JobAnswered: true}, want: Unproved, why: "pending start job"},
		{name: "Job unanswered", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{ActiveState: "inactive", SubState: "dead", Result: "success", MainPID: "0"}, want: Unknown,
			why: "did not answer Job"},
		{name: "ActiveState unanswered", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{MainPID: "0", JobAnswered: true}, want: Unknown, why: "did not answer ActiveState"},
		{name: "MainPID unanswered", rec: rec(ended("i1", Drained, false)), invocation: "i1",
			after: Unit{ActiveState: "inactive", JobAnswered: true}, want: Unknown, why: "did not answer MainPID"},
		{name: "the record unreadable", rec: rec(), readErr: errors.New("bad"), invocation: "i1", after: cleanUnit,
			want: Unknown, why: "could not be read", acknowledge: "--reset-unreadable"},
		{name: "no invocation", rec: rec(ended("", Drained, false)), after: cleanUnit, want: Unknown,
			why: "invocation is not known"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Judge(tc.rec, tc.readErr, tc.invocation, tc.after, tc.drainRequired, "boot-a")
			if got.Verdict != tc.want || got.Proof != tc.proof || !strings.Contains(got.Why, tc.why) ||
				got.Answer != tc.want.String() || got.Acknowledge != tc.acknowledge || got.Floor != tc.floor {
				t.Fatalf("Judge = %s proof %q acknowledge %q floor %v (%s), want %s proof %q acknowledge %q "+
					"floor %v saying %q", got.Verdict, got.Proof, got.Acknowledge, got.Floor, got.Why, tc.want,
					tc.proof, tc.acknowledge, tc.floor, tc.why)
			}
		})
	}
}

func acknowledged(e Entry) Entry {
	e.Acknowledged = true

	return e
}

func probeEntry(invocation string) Entry {
	e := ended(invocation, Running, false)
	e.Role = RoleProbe

	return e
}

// A PROBE'S ENTRY IS NEVER READ AS THE NODE'S: a probe drained under the same
// invocation proves nothing about the node's stop.
func TestAProbeEntryProvesNothingAboutTheNode(t *testing.T) {
	probe := ended("i1", Drained, false)
	probe.Role = RoleProbe

	got := Judge(rec(ended("i1", Running, false), probe), nil, "i1", cleanUnit, false, "boot-a")
	if got.Verdict != Unproved {
		t.Fatalf("a probe's drain proved the node's stop: %s (%s)", got.Verdict, got.Why)
	}
}

// A START IS ADMITTED BY WHAT THE NODE PROCESSES LEFT: a drain, a handoff, an
// acknowledgement, or a reboot since a host-run node; an idle ending and
// systemd's clean stop of a process that kept no record pass the question to
// the process before them, so neither covers a killed one; a killed process, a
// cut-short drain or a supersession is refused naming the acknowledgement of
// the newest entry; with no node entry, systemd's account of the unit decides;
// and a probe never counts.
func TestAStartIsAdmittedByWhatTheNodeLeft(t *testing.T) {
	otherBoot := ended("i0", Running, false)
	otherBoot.BootID = "boot-old"

	remoteOtherBoot := otherBoot
	remoteOtherBoot.RunsOnHost = false

	for _, tc := range []struct {
		name        string
		rec         Record
		readErr     error
		now         Unit
		boot        string
		want        Verdict
		proof       string
		why         string
		acknowledge string
	}{
		{name: "a drain", rec: rec(ended("i1", Drained, false)), now: cleanUnit, boot: "boot-a", want: Proved,
			proof: ProofRecord, why: "drained"},
		{name: "a handoff", rec: rec(ended("i1", HandedOff, false)), now: cleanUnit, boot: "boot-a", want: Proved,
			proof: ProofRecord, why: "handed its compute"},
		{name: "killed", rec: rec(ended("i1", Running, false)), now: timedOut, boot: "boot-a", want: Unproved,
			why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "killed, then reset-failed", rec: rec(ended("i1", Running, false)), now: resetFailed,
			boot: "boot-a", want: Unproved, why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "cut short", rec: rec(ended("i1", LeftRunning, false)), now: cleanUnit, boot: "boot-a",
			want: Unproved, why: "left-running", acknowledge: "--invocation i1"},
		{name: "acknowledged", rec: rec(acknowledged(ended("i1", Running, false))), now: timedOut, boot: "boot-a",
			want: Proved, proof: ProofAcknowledged, why: "acknowledged"},
		{name: "idle over a killed process", rec: rec(ended("i0", Running, false), ended("i1", Idle, false)),
			now: cleanUnit, boot: "boot-a", want: Unproved, why: `ended idle; before it, invocation "i0"`,
			acknowledge: "--invocation i1"},
		{name: "an acknowledged idle over a killed process",
			rec: rec(ended("i0", Running, false), acknowledged(ended("i1", Idle, false))), now: cleanUnit,
			boot: "boot-a", want: Proved, proof: ProofAcknowledged, why: "acknowledged"},
		{name: "idle over a drain", rec: rec(ended("i0", Drained, false), ended("i1", Idle, false)), now: cleanUnit,
			boot: "boot-a", want: Proved, proof: ProofRecord, why: `invocation "i0" (pid 9`},
		{name: "systemd's clean stop over a drain", rec: rec(ended("i0", Drained, false), systemdStopped("i1")),
			now: cleanUnit, boot: "boot-a", want: Proved, proof: ProofSystemd, why: "kept no record"},
		{name: "systemd's clean stop over a killed process",
			rec: rec(ended("i0", Running, false), systemdStopped("i1")), now: cleanUnit, boot: "boot-a",
			want: Unproved, why: "killed or crashed", acknowledge: "--invocation i1"},
		{name: "systemd's clean stop over an acknowledgement",
			rec: rec(acknowledged(ended("i0", Running, false)), systemdStopped("i1")), now: cleanUnit,
			boot: "boot-a", want: Proved, proof: ProofSystemd, why: "acknowledged"},
		{name: "systemd's clean stop over a reboot", rec: rec(otherBoot, systemdStopped("i2")), now: cleanUnit,
			boot: "boot-a", want: Proved, proof: ProofSystemd, why: "earlier boot"},
		{name: "systemd's clean stop alone", rec: rec(systemdStopped("i1")), now: cleanUnit, boot: "boot-a",
			want: Proved, proof: ProofSystemd, why: "no earlier process is recorded"},
		{name: "systemd's failed stop", rec: rec(Entry{InvocationID: "i1", Role: RoleNode, Source: SourceSystemd,
			Ending: Failed}), now: cleanUnit, boot: "boot-a", want: Unproved, why: "ended failed",
			acknowledge: "--invocation i1"},
		{name: "a reboot since a host-run node", rec: rec(otherBoot), now: cleanUnit, boot: "boot-a", want: Proved,
			proof: ProofReboot, why: "earlier boot"},
		{name: "a reboot under an idle process", rec: rec(otherBoot, ended("i1", Idle, false)), now: cleanUnit,
			boot: "boot-a", want: Proved, proof: ProofReboot, why: "earlier boot"},
		{name: "a reboot since a remote node", rec: rec(remoteOtherBoot), now: cleanUnit, boot: "boot-a",
			want: Unproved, why: "killed or crashed", acknowledge: "--invocation i0"},
		{name: "the boot unknown", rec: rec(otherBoot), now: cleanUnit, want: Unproved, why: "killed or crashed",
			acknowledge: "--invocation i0"},
		{name: "a probe is not the newest", rec: rec(ended("i1", Running, false), probeEntry("p1")), now: cleanUnit,
			boot: "boot-a", want: Unproved, why: `"i1"`, acknowledge: "--invocation i1"},
		{name: "an unproved process with no invocation", rec: rec(ended("", LeftRunning, false)), now: cleanUnit,
			boot: "boot-a", want: Unproved, why: "left-running", acknowledge: "--unrecorded"},
		{name: "no record, systemd clean", rec: rec(), now: cleanUnit, boot: "boot-a", want: Proved,
			proof: ProofSystemd, why: "no ending"},
		{name: "no record, systemd timeout", rec: rec(), now: timedOut, boot: "boot-a", want: Unproved,
			why: "failed/failed/timeout", acknowledge: "--unrecorded"},
		{name: "a process running", rec: rec(ended("i1", Drained, false)),
			now:  Unit{ActiveState: "active", SubState: "running", Result: "success", MainPID: "5", JobAnswered: true},
			boot: "boot-a", want: Unproved, why: "is active"},
		{name: "unreadable", rec: rec(), readErr: errors.New("bad"), now: cleanUnit, boot: "boot-a", want: Unknown,
			why: "could not be read", acknowledge: "--reset-unreadable"},
		{name: "not this user's to read", rec: rec(), readErr: fmt.Errorf("read: %w", fs.ErrPermission),
			now: cleanUnit, boot: "boot-a", want: Unknown, why: "judge it as root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := AdmitStart(tc.rec, tc.readErr, tc.now, tc.boot)
			if got.Verdict != tc.want || got.Proof != tc.proof || !strings.Contains(got.Why, tc.why) ||
				got.Acknowledge != tc.acknowledge {
				t.Fatalf("AdmitStart = %s proof %q acknowledge %q (%s), want %s proof %q acknowledge %q saying %q",
					got.Verdict, got.Proof, got.Acknowledge, got.Why, tc.want, tc.proof, tc.acknowledge, tc.why)
			}
		})
	}
}

// A STOP THAT FOUND NOTHING TO STOP IS JUDGED BY WHAT THE NEWEST PROCESS LEFT:
// a second `local down` over a node an earlier one stopped unproved is refused
// again, a drain required refuses a handoff and an acknowledgement (which
// admits a start only), and a drain passes.
func TestAStopOfAStoppedNodeIsJudgedByWhatItLeft(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rec   Record
		drain bool
		want  Verdict
		why   string
	}{
		{"a drain, a drain required", rec(ended("i1", Drained, false)), true, Proved, "drained"},
		{"killed, a drain required", rec(ended("i1", Running, false)), true, Unproved, "killed or crashed"},
		{"a handoff, a drain required", rec(ended("i1", HandedOff, false)), true, Unproved, "never adopted it"},
		{"a handoff, no drain required", rec(ended("i1", HandedOff, false)), false, Proved, "handed its compute"},
		{"acknowledged, a drain required", rec(acknowledged(ended("i1", Running, false))), true, Unproved,
			"killed or crashed"},
		{"acknowledged, no drain required", rec(acknowledged(ended("i1", Running, false))), false, Proved,
			"acknowledged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Standing(tc.rec, nil, cleanUnit, tc.drain, "boot-a")
			if got.Verdict != tc.want || !strings.Contains(got.Why, tc.why) {
				t.Fatalf("Standing = %s (%s), want %s saying %q", got.Verdict, got.Why, tc.want, tc.why)
			}
		})
	}
}
