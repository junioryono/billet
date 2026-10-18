package host

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/retirement"
)

// repairFixture is the endpoint fixture holding the skew #168 describes: an R
// process, no record, and a node unit the role rendered before the record
// existed, both as systemd has loaded it and as the file on disk. The stop,
// the reload and the start are staged to succeed; each test breaks one thing.
type repairFixture struct {
	*endpointFixture
	unitPath string
	legacy   string
	// locks counts the lifecycle locks the repair took.
	locks int
}

// repairHolder is the converge whose guard the fixture holds.
const repairHolder = "h1"

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()

	f := &repairFixture{endpointFixture: newEndpointFixture(t)}
	f.wait = endpointExpiryWait

	// THIS CONVERGE'S GUARD, which the repair runs under; the managed binary
	// the guard fixture names is then the R release the node runs.
	newGuardFixture(t)
	mustHold(t, repairHolder)
	pinRetirementRoot(t)
	f.rBinary(t)
	mustOK(t, os.Remove(f.recordPath))

	// THE OLD COLLECTION'S UNIT: the lock directory as its only runtime
	// directory, which the shipped unit no longer declares there (#175).
	f.legacy = strings.Replace(deploy.NodeUnit, "RuntimeDirectory=billet/registration\n",
		"RuntimeDirectory=billet/locks\n", 1)
	if f.legacy == deploy.NodeUnit {
		t.Fatal("the shipped node unit declares no RuntimeDirectory line to make legacy")
	}

	f.unitPath = filepath.Join(t.TempDir(), nodeUnit)
	writeFile(t, f.unitPath, f.legacy, 0o644)
	t.Setenv("BILLET_FAKE_UNIT_FILE", f.unitPath)

	prevPath, prevOwned, prevLock := repairUnitPath, repairUnitOwned, lifecycleLock
	repairUnitPath = f.unitPath
	repairUnitOwned = func(os.FileInfo) error { return nil }
	lifecycleLock = func() (*hostauthority.LifecycleLock, error) {
		f.locks++

		return &hostauthority.LifecycleLock{}, nil
	}

	t.Cleanup(func() { repairUnitPath, repairUnitOwned, lifecycleLock = prevPath, prevOwned, prevLock })

	f.loaded(t, nodeUnit, "active", "running", inspectPID, nodeInvocation, "billet/locks")
	f.loaded(t, nodeUnit+".after-stop", "inactive", "dead", 0, nodeInvocation, "billet/locks")
	f.loaded(t, nodeUnit+".after-reload", "inactive", "dead", 0, nodeInvocation, "billet/locks billet/registration")
	f.loaded(t, nodeUnit+".after-start", "active", "running", inspectPID+1, newInvocation,
		"billet/locks billet/registration")
	f.recordAfterStart(t, canonicalA)

	return f
}

// pinRetirementRoot puts the retirement journal's root in a directory of the
// test's own, so a repair reads no host's journal.
func pinRetirementRoot(t *testing.T) {
	t.Helper()

	prev := retirement.Root
	retirement.Root = t.TempDir()

	t.Cleanup(func() { retirement.Root = prev })
}

// loaded writes one of the fake's unit answers: the node unit as systemd has
// loaded it from the role's fragment, with the RuntimeDirectory given.
func (f *repairFixture) loaded(t *testing.T, name, active, sub string, pid int, invocation, runtime string) {
	t.Helper()

	body := strings.Replace(nodeUnitBody(active, sub, pid, invocation, "mixed", "success"),
		"RuntimeDirectory=billet/locks billet/registration\n", "RuntimeDirectory="+runtime+"\n", 1)
	writeFile(t, f.unitFile(name), body+"FragmentPath="+f.unitPath+"\nDropInPaths=\n", 0o644)
}

// set replaces one property in one of the fake's answers for the node unit.
func (f *repairFixture) set(t *testing.T, name, property, value string) {
	t.Helper()

	path := f.unitFile(name)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(string(body), "\n")
	found := false

	for i, line := range lines {
		if strings.HasPrefix(line, property+"=") {
			lines[i], found = property+"="+value, true
		}
	}

	if !found {
		t.Fatalf("the fake answers no %s for %s", property, name)
	}

	writeFile(t, path, strings.Join(lines, "\n"), 0o644)
}

func (f *repairFixture) repair(t *testing.T, extra ...string) endpointOut {
	t.Helper()

	return runEndpoint(t, NodeRepairUnit, "", append([]string{"--config", f.configPath, "--wait", f.wait,
		"--stop-timeout", "1m", "--run", repairHolder}, extra...)...)
}

// acted is every systemctl verb other than show the fake was asked for, in
// order.
func (f *repairFixture) acted(t *testing.T) []string {
	t.Helper()

	body, err := os.ReadFile(f.logPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	var verbs []string

	for line := range strings.Lines(string(body)) {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] != "show" {
			verbs = append(verbs, fields[0])
		}
	}

	return verbs
}

func (f *repairFixture) unitOnDisk(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(f.unitPath)
	if err != nil {
		t.Fatal(err)
	}

	return string(body)
}

// writeRecordAfterStart stages the record the fake publishes at the start,
// with fields of the test's choosing over the started invocation's.
func (f *repairFixture) writeRecordAfterStart(t *testing.T, fields map[string]any) {
	t.Helper()

	rec := f.record(map[string]any{"deployment": f.deployment, "invocation_id": newInvocation,
		"incarnation": newIncarnation})
	maps.Copy(rec, fields)

	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, f.unitFile("record.after-start"), string(body)+"\n", 0o600)
}

// THE REPAIR CHANGES ONE LINE AND ENDS WITH THE NEW INVOCATION'S RECORD. The
// node is stopped under the migration's proof, the unit installed with only
// RuntimeDirectory changed, systemd reloaded, the node started, and the
// answer carries the record the started invocation wrote, naming the
// installed endpoint.
func TestTheRepairChangesOneLineAndEndsWithTheNewInvocationsRecord(t *testing.T) {
	f := newRepairFixture(t)

	o := f.repair(t)
	if o.code != 0 || o.str("outcome") != outcomeRepaired {
		t.Fatalf("code %d outcome %q (why %q), want repaired", o.code, o.str("outcome"), o.str("why"))
	}

	want := strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
		"RuntimeDirectory=billet/locks billet/registration\n", 1)
	if got := f.unitOnDisk(t); got != want {
		t.Errorf("the repaired unit is not the one it was with only RuntimeDirectory changed:\n%s", got)
	}

	if got := f.acted(t); !slices.Equal(got, []string{"stop", "daemon-reload", "start"}) {
		t.Errorf("systemctl was asked to %q, want a stop, a reload and a start, in that order", got)
	}

	if o.str("from_invocation") != nodeInvocation || o.str("invocation_id") != newInvocation ||
		o.str("endpoint") != canonicalA {
		t.Errorf("the evidence names invocation %s to %s on %s, want %s to %s on %s", o.str("from_invocation"),
			o.str("invocation_id"), o.str("endpoint"), nodeInvocation, newInvocation, canonicalA)
	}

	if o.str("before_sha256") != digestHex([]byte(f.legacy)) || o.str("after_sha256") != digestHex([]byte(want)) {
		t.Error("the evidence's digests are not the unit's before and after")
	}

	// THE STOP IS ADMITTED ON ONE ANSWER: the last read before it asks for the
	// process and the unit's shape together, and it is the seventh, after the
	// two judgements' three each, as the staged reviews of it count.
	shows, stop := f.showsBeforeStop(t)
	if len(shows) != 7 {
		t.Fatalf("systemd was asked %d times before the stop, want 7: %q", len(shows), shows)
	}

	for _, property := range []string{"MainPID", "InvocationID", "KillMode", "FragmentPath", "DropInPaths",
		"NeedDaemonReload", "RuntimeDirectory"} {
		if !strings.Contains(shows[6], "--property="+property) {
			t.Errorf("the stop's admission (%q) did not ask for %s on the same answer", shows[6], property)
		}
	}

	if !stop {
		t.Error("the repair asked systemctl to stop nothing")
	}
}

// showsBeforeStop is every show the fake answered before the first stop, and
// whether a stop was asked for at all.
func (f *repairFixture) showsBeforeStop(t *testing.T) ([]string, bool) {
	t.Helper()

	body, err := os.ReadFile(f.logPath)
	if err != nil {
		t.Fatal(err)
	}

	var shows []string

	for line := range strings.Lines(string(body)) {
		switch fields := strings.Fields(line); {
		case len(fields) == 0:
		case fields[0] == "stop":
			return shows, true
		case fields[0] == "show":
			shows = append(shows, strings.TrimSpace(line))
		}
	}

	return shows, false
}

// THE DRY RUN SAYS WHAT A CONVERGE WOULD DO AND DOES NOTHING: the repair is
// required, the effective endpoint is unknown, and the unit and the node are as
// they were.
func TestTheRepairsDryRunChangesNothing(t *testing.T) {
	f := newRepairFixture(t)

	o := f.repair(t, "--dry-run")
	if o.code != 0 || o.str("outcome") != outcomeReported || !o.boolean("required") ||
		!strings.Contains(o.str("why"), "runtime-unit repair required; effective endpoint unknown") {
		t.Fatalf("code %d outcome %q required %v why %q, want a required repair reported", o.code, o.str("outcome"),
			o.boolean("required"), o.str("why"))
	}

	if got := f.acted(t); len(got) != 0 {
		t.Errorf("a dry run asked systemctl to %q", got)
	}

	if f.unitOnDisk(t) != f.legacy {
		t.Error("a dry run changed the unit file")
	}
}

// A UNIT THAT DECLARES THE DIRECTORY, OR A NODE THAT IS NOT RUNNING, NEEDS NO
// REPAIR: the dry run says so and the action answers unchanged, touching
// nothing.
func TestTheRepairIsNotRequiredWhereTheRecordCanBeHeld(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, f *repairFixture)
	}{
		{"the unit declares the directory", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.loaded(t, nodeUnit, "active", "running", inspectPID, nodeInvocation, "billet/locks billet/registration")
		}},
		{"the node is not running", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.loaded(t, nodeUnit, "inactive", "dead", 0, "", "billet/locks")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			if o := f.repair(t, "--dry-run"); o.code != 0 || o.str("outcome") != outcomeReported || o.boolean("required") {
				t.Fatalf("the dry run answered code %d outcome %q required %v (why %q), want not required", o.code,
					o.str("outcome"), o.boolean("required"), o.str("why"))
			}

			if o := f.repair(t); o.code != 0 || o.str("outcome") != outcomeUnchanged {
				t.Fatalf("the action answered code %d outcome %q (why %q), want unchanged", o.code, o.str("outcome"),
					o.str("why"))
			}

			if got := f.acted(t); len(got) != 0 || f.unitOnDisk(t) != f.legacy {
				t.Errorf("an unrequired repair asked systemctl to %q or changed the unit", got)
			}
		})
	}
}

// ONLY THE KNOWN LEGACY SHAPE IS ADMITTED, and a refusal stops nothing: a
// drop-in, a unit loaded from elsewhere, a reload pending, another
// RuntimeDirectory, a file not root's, a file whose line is not the admitted
// one or carries the directive twice, and a file systemd would read
// differently.
func TestTheRepairAdmitsOnlyTheLegacyShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stage   func(t *testing.T, f *repairFixture)
		outcome string
	}{
		{"a drop-in", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "DropInPaths", "/etc/systemd/system/billet-node.service.d/x.conf")
		}, outcomeRefused},
		{"loaded from the package", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "FragmentPath", "/usr/lib/systemd/system/billet-node.service")
		}, outcomeRefused},
		{"a reload pending", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "NeedDaemonReload", "yes")
		}, outcomeUnknown},
		{"another runtime directory", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "RuntimeDirectory", "billet/locks billet/other")
		}, outcomeRefused},
		{"not root's", func(t *testing.T, f *repairFixture) {
			t.Helper()
			repairUnitOwned = func(os.FileInfo) error { return errors.New(f.unitPath + " is not owned by root") }
		}, outcomeRefused},
		{"the line is another", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitPath, strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
				"RuntimeDirectory=billet/locks \n", 1), 0o644)
		}, outcomeRefused},
		{"the directive twice", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitPath, strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
				"RuntimeDirectory=billet/locks\nRuntimeDirectory=\n", 1), 0o644)
		}, outcomeRefused},
		{"a continuation", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitPath, strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
				"Environment=A=1 \\\nRuntimeDirectory=billet/locks\n", 1), 0o644)
		}, outcomeRefused},
		{"a carriage return", func(t *testing.T, f *repairFixture) {
			t.Helper()
			// systemd ends the comment at the carriage return and reads [Unit]
			// as a header, so the directive below it is not in [Service].
			writeFile(t, f.unitPath, strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
				"# note\r[Unit]\nRuntimeDirectory=billet/locks\n", 1), 0o644)
		}, outcomeRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			o := f.repair(t)
			if o.str("outcome") != tc.outcome || o.str("state") != stateNothing || o.str("reason") != endpointReasonShape {
				t.Fatalf("outcome %q state %q (reason %q, why %q), want %s %s with nothing done", o.str("outcome"),
					o.str("state"), o.str("reason"), o.str("why"), tc.outcome, endpointReasonShape)
			}

			if got := f.acted(t); len(got) != 0 {
				t.Errorf("a refused repair asked systemctl to %q", got)
			}
		})
	}
}

// A REFUSED SHAPE NAMES THE REMEDY THAT CHANGES IT, never the package for a
// unit the package cannot reach: a unit between the role's and the package's
// is outranked by a converge once the node is stopped; the role's own unit in
// another shape is rendered afresh the same way; a packaged one is replaced by
// the package; and a drop-in is removed first, after which a converge judges
// the unit again. Each case names the refusal it reaches, so a remedy shared
// by several refusals cannot pass for another's.
func TestTheRepairsRefusalNamesTheRemedyThatReachesTheUnit(t *testing.T) {
	const (
		stopFirst = "stop billet-node.service, then converge the host role"
		pkg       = "install this release's package"
		judges    = "; then converge the host role, which judges the unit without them"
	)

	for _, tc := range []struct {
		name         string
		stage        func(t *testing.T, f *repairFixture)
		why          string
		want, absent []string
	}{
		{"a runtime unit", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "FragmentPath", "/run/systemd/system/billet-node.service")
		}, "is loaded from /run/systemd/system/billet-node.service",
			[]string{stopFirst, "whose unit under /etc/systemd/system outranks that one"},
			[]string{pkg, "renders that unit afresh"}},
		{"the role's unit in another shape", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "RuntimeDirectory", "billet/locks billet/other")
		}, "declares RuntimeDirectory=billet/locks billet/other", []string{stopFirst, "renders that unit afresh"},
			[]string{pkg}},
		{"the role's unit with a drop-in", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "DropInPaths", "/etc/systemd/system/billet-node.service.d/env.conf")
		}, "has drop-ins", []string{"remove the drop-ins that apply to billet-node.service " +
			"(/etc/systemd/system/billet-node.service.d/env.conf", "then systemctl daemon-reload", judges},
			[]string{pkg, stopFirst}},
		{"the role's unit in another shape with a drop-in", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "RuntimeDirectory", "billet/locks billet/cache")
			f.set(t, nodeUnit, "DropInPaths", "/etc/systemd/system/billet-node.service.d/rt.conf")
		}, "declares RuntimeDirectory=billet/locks billet/cache", []string{"remove the drop-ins that apply to " +
			"billet-node.service (/etc/systemd/system/billet-node.service.d/rt.conf", judges},
			[]string{pkg, stopFirst}},
		{"the role's file not root's", func(t *testing.T, f *repairFixture) {
			t.Helper()
			repairUnitOwned = func(os.FileInfo) error { return errors.New(f.unitPath + " is not owned by root") }
		}, "is not owned by root", []string{stopFirst, "renders that unit afresh"},
			[]string{pkg, "outranks that one"}},
		{"the role's file in another line shape", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitPath, strings.Replace(f.legacy, "RuntimeDirectory=billet/locks\n",
				"RuntimeDirectory=billet/locks \n", 1), 0o644)
		}, "does not hold exactly one RuntimeDirectory assignment", []string{stopFirst}, []string{pkg}},
		{"an alias", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "FragmentPath", "/etc/systemd/system/other.service")
		}, "is loaded from /etc/systemd/system/other.service",
			[]string{"billet-node.service is an alias of other.service"}, []string{pkg, stopFirst}},
		{"a packaged unit", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit, "FragmentPath", "/usr/lib/systemd/system/billet-node.service")
		}, "is loaded from /usr/lib/systemd/system/billet-node.service", []string{pkg}, []string{"by hand"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			o := f.repair(t)
			if o.str("outcome") != outcomeRefused || o.str("reason") != endpointReasonShape ||
				!strings.Contains(o.str("why"), tc.why) {
				t.Fatalf("outcome %q reason %q why %q, want the shape refusal saying %q", o.str("outcome"),
					o.str("reason"), o.str("why"), tc.why)
			}

			for _, clause := range tc.want {
				if !strings.Contains(o.str("next"), clause) {
					t.Errorf("next %q does not say %q", o.str("next"), clause)
				}
			}

			for _, clause := range tc.absent {
				if strings.Contains(o.str("next"), clause) {
					t.Errorf("next %q says %q, which is not this refusal's remedy", o.str("next"), clause)
				}
			}
		})
	}
}

// EVERY STEP THAT FAILS SAYS WHAT IT LEFT. A stop that is not proved leaves the
// unit untouched; a unit file changed under the stop is not replaced; a
// reload that fails, or one after which systemd does not load the repair,
// leaves the unit installed and the node stopped; a start that fails says the
// same; and a started node whose record is absent, written by the invocation
// before, or naming another endpoint is not a repair.
func TestTheRepairNamesTheStateEachFailureLeaves(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stage    func(t *testing.T, f *repairFixture)
		state    string
		repaired bool // whether the unit on disk is the repaired one
	}{
		{"the stop timed out", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit+".after-stop", "Result", "timeout")
			f.set(t, nodeUnit+".after-stop", "ActiveState", "failed")
			f.set(t, nodeUnit+".after-stop", "SubState", "failed")
		}, stateStopped, false},
		{"the unit file changed under the stop", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitFile("stop.mode"), "edit-unit-file\n", 0o644)
		}, stateStopped, false},
		{"the reload failed", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitFile("reload.mode"), "fail\n", 0o644)
		}, stateUnitInstalled, true},
		{"systemd did not load the repair", func(t *testing.T, f *repairFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.unitFile(nodeUnit+".after-reload")))
		}, stateUnitInstalled, true},
		{"a drop-in loaded with the repair", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit+".after-reload", "DropInPaths", "/etc/systemd/system/billet-node.service.d/x.conf")
		}, stateUnitInstalled, true},
		{"a reload still pending after the repair", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.set(t, nodeUnit+".after-reload", "NeedDaemonReload", "yes")
		}, stateUnitInstalled, true},
		{"the start failed", func(t *testing.T, f *repairFixture) {
			t.Helper()
			writeFile(t, f.unitFile("start.mode"), "fail\n", 0o644)
		}, stateUnitInstalled, true},
		{"a start whose proof failed with a process running", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.loaded(t, nodeUnit+".after-start", "activating", "start", inspectPID+1, newInvocation,
				"billet/locks billet/registration")
		}, stateStartedNoRecord, true},
		{"no record after the start", func(t *testing.T, f *repairFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.unitFile("record.after-start")))
		}, stateStartedNoRecord, true},
		{"the record is the invocation before's", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.writeRecordAfterStart(t, map[string]any{"endpoint": canonicalA, "invocation_id": nodeInvocation})
		}, stateStartedNoRecord, true},
		{"the record names another endpoint", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.recordAfterStart(t, canonicalB)
		}, stateStarted, true},
		{"the start answers the invocation before", func(t *testing.T, f *repairFixture) {
			t.Helper()
			// A record naming the invocation before would be its own, so only the
			// new invocation's identity tells the start's record from it.
			f.loaded(t, nodeUnit+".after-start", "active", "running", inspectPID, nodeInvocation,
				"billet/locks billet/registration")
			f.writeRecordAfterStart(t, map[string]any{"endpoint": canonicalA, "invocation_id": nodeInvocation})
		}, stateStartedNoRecord, true},
		{"the record names another node", func(t *testing.T, f *repairFixture) {
			t.Helper()
			f.writeRecordAfterStart(t, map[string]any{"endpoint": canonicalA, "node": "node-b"})
		}, stateStartedNoRecord, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepairFixture(t)
			tc.stage(t, f)

			o := f.repair(t)
			if o.str("outcome") != outcomeUnknown || o.str("state") != tc.state {
				t.Fatalf("outcome %q state %q (reason %q, why %q), want unknown leaving %s", o.str("outcome"),
					o.str("state"), o.str("reason"), o.str("why"), tc.state)
			}

			if tc.name == "the record names another node" && !strings.Contains(o.str("why"), "cannot be judged") {
				t.Errorf("a foreign record was answered %q, want it named as one that cannot be judged", o.str("why"))
			}

			// THE STEP THAT FAILED IS THE ONE NAMED: a failed reload leaves the
			// same state as a reload that loaded something else.
			if tc.name == "the reload failed" && !strings.Contains(o.str("why"), "reload systemd") {
				t.Errorf("a failed reload was answered %q, want the reload named", o.str("why"))
			}

			repaired := strings.Contains(f.unitOnDisk(t), "RuntimeDirectory=billet/locks billet/registration\n")
			if repaired != tc.repaired {
				t.Errorf("the unit on disk is repaired=%v, want %v", repaired, tc.repaired)
			}
		})
	}
}
