package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func settledCheckArgs(t *testing.T, f *requestFixture, j retirement.Journal, purpose string) []string {
	t.Helper()
	return []string{"--check-" + purpose, "--run", requestRun, "--retiring-host", requestRetiring,
		"--transition", j.Provenance.TransitionID, "--expected-holder", requestRun, "--expected-guard", f.guard.record(t).ID}
}

func settledExpectation(t *testing.T, f *requestFixture, j retirement.Journal, purpose string) retirement.SettledVerdict {
	t.Helper()
	return retirement.SettledVerdict{Purpose: purpose, Run: requestRun, Guard: f.guard.record(t).ID,
		Retiring: requestRetiring, Deployment: j.Deployment, TransitionID: j.Provenance.TransitionID, Variant: j.Variant}
}

// File identity and timestamps catch even a rewrite of the same bytes.
func forbidSettledWrites(t *testing.T, f *requestFixture) {
	t.Helper()
	forbidNodeConfigWrites(t, f)
	for _, path := range []string{f.cfg, retirement.JournalPath(), retirement.StatusPath(), retirement.ServiceAccountPath(),
		filepath.Join(f.guard.active(), guardRecordName), receiptPath} {
		before, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			t.Cleanup(func() {
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("read-only mode created %s: %v", path, err)
				}
			})
			continue
		}
		mustOK(t, err)
		t.Cleanup(func() {
			after, err := os.Lstat(path)
			mustOK(t, err)
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || before.Mode() != after.Mode() {
				t.Fatalf("read-only mode rewrote %s", path)
			}
		})
	}
}

func quietSettledNode(t *testing.T, f *requestFixture, activity string) {
	t.Helper()
	f.manager.set(nodeUnit, "ActiveState", activity)
	f.manager.set(nodeUnit, "MainPID", "0")
	f.manager.set(nodeUnit, "InvocationID", "")
	if err := os.Remove(registrationRecordPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
}

// Replacing entry with strict done strands both interrupted quiet states.
// These are command witnesses; the two-main role witness belongs to commit 3.
func TestRetirementSettledEntryAdmitsActiveAndQuietWithoutWrites(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	args := settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)
	want := settledExpectation(t, f, j, retirement.PurposeSettledEntry)
	for _, activity := range []string{"active", "inactive", "failed"} {
		t.Run(activity, func(t *testing.T) {
			if activity != "active" {
				quietSettledNode(t, f, activity)
			}
			t.Setenv("BILLET_STATE_DSN", "postgres://billet@127.0.0.1:1/unreachable?sslmode=disable")
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", args...)
			verdict, err := retirement.DecodeSettledVerdict([]byte(out), code, want)
			mustOK(t, err)
			expected := activity
			if activity != "active" {
				expected = "quiet-" + activity
			}
			if verdict.NodeActivity != expected || verdict.CompletedBy != j.CompletedBy || strings.Contains(out, "postconditions") {
				t.Fatalf("entry claimed a tail proof: %s", out)
			}
			var done retireDoneAnswer
			if err := retirement.DecodeDocument([]byte(out), &done); err == nil {
				t.Fatal("entry answer parsed as the normal done answer")
			}
			closing := want
			closing.Purpose = retirement.PurposeSettledClosing
			if _, err := retirement.DecodeSettledVerdict([]byte(out), code, closing); err == nil {
				t.Fatal("entry answer parsed as strict closing")
			}
		})
	}
}

// Removing only the active branch's strict proof admits an invocation that
// has never registered; closing's separate proof cannot mask this omission.
func TestRetirementSettledActiveEntryRequiresCurrentRegistration(t *testing.T) {
	for _, registration := range []string{"missing", "stale", "current"} {
		t.Run(registration, func(t *testing.T) {
			f, j := settledNodeConfigFixture(t)
			before := mustRead(t, registrationRecordPath)
			f.svc.onStart = nil
			restartRetireProofNode(t, f, registration)
			if registration == "stale" && mustRead(t, registrationRecordPath) != before {
				t.Fatal("stale registration witness published a new record")
			}
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)...)
			if registration == "current" {
				verdict, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, retirement.PurposeSettledEntry))
				if err != nil || verdict.NodeActivity != "active" {
					t.Fatalf("registered active entry refused: %s (%v)", out, err)
				}
				return
			}
			refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, retirement.PurposeSettledEntry)
			if err != nil || code != exitUnknown || refusal.Reason != retireReasonPostcondition ||
				!strings.Contains(refusal.Why, "retained-node-registration-unproved") {
				t.Fatalf("unregistered active entry admitted or refused elsewhere: %s (%v)", out, err)
			}
		})
	}
}

// Trimming any scalar turns malformed evidence into an otherwise healthy
// observation. Exercise both active and quiet branches at the command edge.
func TestRetirementSettledModesRejectPaddedNodeEvidence(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, activity := range []string{"active", "inactive", "failed"} {
		if activity != "active" {
			quietSettledNode(t, f, activity)
		}
		for _, property := range []string{"LoadState", "ActiveState", "MainPID", "ControlPID", "UnitFileState", "NeedDaemonReload", "User", "Group", "Id", "FragmentPath", "SourcePath", "DropInPaths", "ControlGroup", "Slice", "Job"} {
			for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
				t.Run(activity+"/"+property+"/"+purpose, func(t *testing.T) {
					path := filepath.Join(f.unitsDir, nodeUnit)
					if property != "LoadState" && property != "ActiveState" && property != "MainPID" && property != "UnitFileState" {
						path += ".effects"
					}
					before := mustRead(t, path)
					t.Cleanup(func() { writeFile(t, path, before, 0o644) })
					props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, property)
					mustOK(t, err)
					if len(props[property]) != 1 {
						t.Fatalf("missing control property %s: %+v", property, props)
					}
					setRetireUnitProperty(t, path, property, props[property][0]+" ")
					forbidSettledWrites(t, f)
					out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, purpose)...)
					refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose)
					want := "settled-entry-node-observation-unreadable"
					if property == "Job" {
						want = "settled-entry-node-job-observation"
					}
					if err != nil || code != exitUnknown || refusal.Reason != want {
						t.Fatalf("padded %s admitted or refused elsewhere: %s (%v)", property, out, err)
					}
				})
			}
		}
	}
}

// NeedDaemonReload=no does not rediscover a newly installed fragment. Omitting
// effective-source admission accepts the override in both command modes.
func TestRetirementSettledModesRequireTheEffectiveInstalledSource(t *testing.T) {
	for _, c := range []struct{ purpose, activity string }{
		{retirement.PurposeSettledEntry, "active"},
		{retirement.PurposeSettledEntry, "inactive"},
		{retirement.PurposeSettledEntry, "failed"},
		{retirement.PurposeSettledClosing, "active"},
	} {
		t.Run(c.purpose+"/"+c.activity, func(t *testing.T) {
			f, j := settledNodeConfigFixture(t)
			if c.activity != "active" {
				quietSettledNode(t, f, c.activity)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			mustOK(t, err)
			persistent := filepath.Join(root, "etc", "systemd", "system")
			runtime := filepath.Join(root, "run", "systemd", "system")
			vendor := filepath.Join(root, "usr", "lib", "systemd", "system", nodeUnit)
			for _, dir := range []string{persistent, runtime, filepath.Dir(vendor)} {
				mustOK(t, os.MkdirAll(dir, 0o755))
			}
			props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "FragmentPath")
			mustOK(t, err)
			body := mustRead(t, firstProp(props, "FragmentPath"))
			writeFile(t, vendor, body, 0o644)
			setRetireEffect(t, f, nodeUnit, "FragmentPath", vendor)
			saved := retireOperationInspector
			retireOperationInspector = func() *lifeops.Inspector {
				i := saved()
				lifeops.WithOperationUnitDirectories(persistent, runtime)(i)
				return i
			}
			t.Cleanup(func() { retireOperationInspector = saved })
			for _, unit := range retireInertUnits {
				path, err := retireInertDropIn(retireOperationInspector(), unit)
				mustOK(t, err)
				writeFile(t, path, retireInertBytes(j), 0o644)
				setRetireEffect(t, f, unit, "DropInPaths", path)
			}
			forbidSettledWrites(t, f)
			args := settledCheckArgs(t, f, j, c.purpose)
			out, code := f.runRaw(t, "", args...)
			if _, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, c.purpose)); err != nil {
				t.Fatalf("vendor source control refused: %s (%v)", out, err)
			}
			override := filepath.Join(persistent, nodeUnit)
			changed := body + "[Service]\nUser=other\n"
			writeFile(t, override, changed, 0o644)
			props, err = retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "FragmentPath", "NeedDaemonReload")
			mustOK(t, err)
			if firstProp(props, "FragmentPath") != vendor || firstProp(props, "NeedDaemonReload") != "no" {
				t.Fatalf("override witness lost its stale loaded source: %+v", props)
			}
			out, code = f.runRaw(t, "", args...)
			refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, c.purpose)
			if err != nil || code != exitUnknown || refusal.Reason != "settled-entry-node-unit-mismatch" ||
				!strings.Contains(refusal.Why, "operation-install-source-inconsistent") {
				t.Fatalf("higher-priority fragment admitted or refused elsewhere: %s (%v)", out, err)
			}
			if mustRead(t, vendor) != body || mustRead(t, override) != changed {
				t.Fatal("read-only source admission rewrote a unit")
			}
		})
	}
}

// Each property is supplied only when requested by the production inspector.
// Omitting Job or accepting an absent property must fail its named case.
func TestRetirementSettledEntryNamesEveryNodeRefusal(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	args := settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)
	quietSettledNode(t, f, "inactive")
	cases := []struct {
		name     string
		property string
		value    string
		reason   string
		code     int
	}{
		{"activating", "ActiveState", "activating", "settled-entry-node-activating", exitRefused},
		{"deactivating", "ActiveState", "deactivating", "settled-entry-node-deactivating", exitRefused},
		{"reloading", "ActiveState", "reloading", "settled-entry-node-reloading", exitRefused},
		{"maintenance", "ActiveState", "maintenance", "settled-entry-node-maintenance", exitRefused},
		{"refreshing", "ActiveState", "refreshing", "settled-entry-node-refreshing", exitRefused},
		{"unknown state", "ActiveState", "future", "settled-entry-node-observation-unreadable", exitUnknown},
		{"absent state", "ActiveState", "", "settled-entry-node-observation-unreadable", exitUnknown},
		{"absent pid", "MainPID", "", "settled-entry-node-observation-unreadable", exitUnknown},
		{"malformed pid", "MainPID", "-1", "settled-entry-node-observation-unreadable", exitUnknown},
		{"noncanonical pid", "MainPID", "00", "settled-entry-node-observation-unreadable", exitUnknown},
		{"repeated pid", "MainPID", "0\nMainPID=0", "settled-entry-node-observation-unreadable", exitUnknown},
		{"missing control pid", "ControlPID", "", "settled-entry-node-observation-unreadable", exitUnknown},
		{"unknown cgroup", "Slice", "other.slice", "settled-entry-node-observation-unreadable", exitUnknown},
		{"process present", "MainPID", "42", "settled-entry-node-process-present", exitRefused},
		{"control process", "ControlPID", "42", "settled-entry-node-process-present", exitRefused},
		{"queued job", "Job", "123 /org/freedesktop/systemd1/job/123", "settled-entry-node-queued-job", exitRefused},
		{"malformed job", "Job", "not-a-job", "settled-entry-node-job-observation", exitUnknown},
		{"blank job", "Job", " ", "settled-entry-node-job-observation", exitUnknown},
		{"tab job", "Job", "\t", "settled-entry-node-job-observation", exitUnknown},
		{"padded queued job", "Job", "123 ", "settled-entry-node-job-observation", exitUnknown},
		{"noncanonical job id", "Job", "0123", "settled-entry-node-job-observation", exitUnknown},
		{"wrong job path", "Job", "123 /org/freedesktop/systemd1/job/124", "settled-entry-node-job-observation", exitUnknown},
		{"missing job", "Job", "", "settled-entry-node-job-observation", exitUnknown},
		{"repeated job", "Job", "\nJob=", "settled-entry-node-job-observation", exitUnknown},
		{"unit mismatch", "NeedDaemonReload", "yes", "settled-entry-node-unit-mismatch", exitRefused},
		{"missing unit", "LoadState", "not-found", "settled-entry-node-observation-unreadable", exitUnknown},
		{"unfamiliar reload", "NeedDaemonReload", "perhaps", "settled-entry-node-observation-unreadable", exitUnknown},
		{"runtime enabled", "UnitFileState", "enabled-runtime", "settled-entry-node-enablement", exitRefused},
		{"unknown enablement", "UnitFileState", "future", "settled-entry-node-observation-unreadable", exitUnknown},
		{"unreadable", "", "", "settled-entry-node-observation-unreadable", exitUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(f.unitsDir, nodeUnit)
			if c.property == "Job" || c.property == "ControlPID" || c.property == "NeedDaemonReload" || c.property == "Slice" {
				path += ".effects"
			}
			before := mustRead(t, path)
			t.Cleanup(func() { writeFile(t, path, before, 0o644) })
			if c.name == "unreadable" {
				mustOK(t, os.Remove(path))
			} else {
				setRetireUnitProperty(t, path, c.property, c.value)
			}
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", args...)
			refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, retirement.PurposeSettledEntry)
			if err != nil || code != c.code || refusal.Reason != c.reason {
				t.Fatalf("%s: expected %s/%d, got %s (%v)", c.name, c.reason, c.code, out, err)
			}
		})
	}
}

// Dropping a record binding or repairing status in the entry path admits the
// corresponding case or changes the independently snapshotted record bytes.
func TestRetirementSettledEntryRequiresEveryProtectedBinding(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	args := settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)
	quietSettledNode(t, f, "inactive")
	for _, scenario := range []string{"status absent", "status malformed", "status stale", "unsettled", "row incomplete", "completion absent",
		"done time malformed", "marker", "holder", "guard id", "preparing", "transaction", "transition", "survivor", "provenance", "archive identity", "identity recreated", "controller active"} {
		t.Run(scenario, func(t *testing.T) {
			paths := []string{retirement.StatusPath(), retirement.JournalPath(), filepath.Join(f.guard.active(), guardRecordName),
				filepath.Join(f.unitsDir, serverUnit)}
			for _, path := range paths {
				body, err := os.ReadFile(path)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				mustOK(t, err)
				info, err := os.Stat(path)
				mustOK(t, err)
				t.Cleanup(func() { writeFile(t, path, string(body), info.Mode().Perm()) })
			}
			change := func(path, member string, value any) {
				var doc map[string]any
				mustOK(t, json.Unmarshal([]byte(mustRead(t, path)), &doc))
				doc[member] = value
				writeFile(t, path, string(mustMarshal(t, doc)), 0o600)
			}
			want := retireReasonJournal
			extra := []string{}
			switch scenario {
			case "status absent":
				mustOK(t, os.Remove(paths[0]))
				want = retireReasonStatus
			case "status malformed":
				writeFile(t, paths[0], "{broken", 0o644)
				want = retireReasonStatus
			case "status stale":
				mustOK(t, retirement.WriteStatus(retirement.PhaseStopped, j.Variant, retireNow()))
				want = retireReasonStatus
			case "unsettled":
				change(paths[1], "settled", false)
			case "row incomplete":
				change(paths[1], "row_done", false)
			case "completion absent":
				change(paths[1], "completed_by", "")
			case "done time malformed":
				change(paths[1], "done_at", "not-a-time")
			case "marker":
				change(paths[2], "transition", map[string]any{"kind": "retirement", "id": j.Provenance.TransitionID})
				want = retireReasonGuard
			case "holder":
				change(paths[2], "holder", "another-run")
				want = retireReasonGuard
			case "guard id":
				change(paths[2], "id", strings.Repeat("e", 32))
				want = retireReasonGuard
			case "preparing":
				change(paths[2], "preparing", true)
				want = retireReasonGuard
			case "transaction":
				mustOK(t, os.Symlink(filepath.Join(f.guard.root, "recovery-foreign"), filepath.Join(f.guard.active(), guardPointerName)))
				t.Cleanup(func() { mustOK(t, os.Remove(filepath.Join(f.guard.active(), guardPointerName))) })
				want = retireReasonGuard
			case "transition":
				extra = []string{"--transition", strings.Repeat("e", 32)}
			case "survivor":
				other := j.Survivor
				other.Host = "other-controller"
				change(paths[1], "survivor", other)
			case "provenance":
				other := j.Provenance
				other.Deployment = strings.Repeat("e", 32)
				change(paths[1], "provenance", other)
			case "archive identity":
				mustOK(t, os.Rename(j.Archive, j.Archive+"-missing"))
				t.Cleanup(func() { mustOK(t, os.Rename(j.Archive+"-missing", j.Archive)) })
				want = retireReasonIdentity
			case "identity recreated":
				mustOK(t, os.Mkdir(j.IdentityDir, 0o700))
				t.Cleanup(func() { mustOK(t, os.Remove(j.IdentityDir)) })
				want = retireReasonIdentity
			case "controller active":
				f.manager.set(serverUnit, "ActiveState", "active")
				want = retireReasonPostcondition
			}
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", append(args, extra...)...)
			refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, retirement.PurposeSettledEntry)
			if err != nil || refusal.Reason != want {
				t.Fatalf("%s admitted or refused at another boundary: %s (%v)", scenario, out, err)
			}
		})
	}
}

// A later authorized holder must not be mistaken for the historical owner.
func TestRetirementSettledEntryAdmitsALaterAuthorizedHolder(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	mustOK(t, guardRun(t, "release", "--holder", requestRun))
	const next = "ordinary-later"
	mustOK(t, guardRun(t, "hold", "--holder", next))
	quietSettledNode(t, f, "inactive")
	forbidSettledWrites(t, f)
	args := settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)
	args = append(args, "--run", next, "--expected-holder", next)
	out, code := f.runRaw(t, "", args...)
	want := settledExpectation(t, f, j, retirement.PurposeSettledEntry)
	want.Run = next
	if _, err := retirement.DecodeSettledVerdict([]byte(out), code, want); err != nil {
		t.Fatalf("later authorized holder refused: %v\n%s", err, out)
	}
}

// Substituting entry permission for closing's strict proof accepts quiet
// states; omitting closing path/unit checks accepts the other drifts.
func TestRetirementSettledClosingRequiresStrictProofAfterEntry(t *testing.T) {
	for _, drift := range []string{"healthy", "inactive", "failed", "controller", "unit", "status"} {
		t.Run(drift, func(t *testing.T) {
			f, j := settledNodeConfigFixture(t)
			out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)...)
			if _, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, retirement.PurposeSettledEntry)); err != nil {
				t.Fatalf("earlier entry failed: %v\n%s", err, out)
			}
			wantReason := retireReasonPostcondition
			switch drift {
			case "inactive", "failed":
				quietSettledNode(t, f, drift)
			case "controller":
				f.manager.set(serverUnit, "ActiveState", "active")
			case "unit":
				setRetireEffect(t, f, nodeUnit, "NeedDaemonReload", "yes")
				wantReason = "settled-entry-node-unit-mismatch"
			case "status":
				mustOK(t, os.Remove(retirement.StatusPath()))
				wantReason = retireReasonStatus
			}
			forbidSettledWrites(t, f)
			out, code = f.runRaw(t, "", settledCheckArgs(t, f, j, retirement.PurposeSettledClosing)...)
			if drift == "healthy" {
				_, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, retirement.PurposeSettledClosing))
				mustOK(t, err)
				return
			}
			refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, retirement.PurposeSettledClosing)
			if err != nil || refusal.Reason != wantReason {
				t.Fatalf("closing accepted %s or refused elsewhere: %s (%v)", drift, out, err)
			}
		})
	}
}

// A produced quiet-entry answer supplies no permission at any write boundary.
// Replacing that boundary's strict predicates with entry eligibility must
// change a protected record and fail here, before any subsequent tail check.
func TestRetirementQuietEntryCannotAuthorizeAnyDoneWrite(t *testing.T) {
	for _, activity := range []string{"inactive", "failed"} {
		for _, boundary := range []string{"done", "status", "marker", "settlement"} {
			t.Run(activity+"/"+boundary, func(t *testing.T) {
				f, j := settledNodeConfigFixture(t)
				quietSettledNode(t, f, activity)
				out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, retirement.PurposeSettledEntry)...)
				if _, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, retirement.PurposeSettledEntry)); err != nil {
					t.Fatalf("quiet entry control refused: %v\n%s", err, out)
				}
				j.Settled = false
				if boundary == "done" {
					j.Phase, j.RowDone, j.CompletedBy, j.DoneAt = retirement.PhaseNodeRestarted, false, "", ""
				}
				mustOK(t, j.Write(retireNow()))
				if boundary == "status" {
					writeFile(t, retirement.StatusPath(), "damaged\n", 0o644)
				}
				var root *txLock
				var dir *os.File
				m := retireProofMode(f)
				if boundary == "marker" {
					markGuard(t, f.guard, &guardTransition{Kind: "retirement", ID: j.Provenance.TransitionID}, nil)
					var r *retireRefusal
					root, dir, _, r = retireGuard(m.run)
					if r != nil {
						t.Fatal(r)
					}
					defer root.release()
					defer func() { _ = dir.Close() }()
				}
				forbidSettledWrites(t, f)
				var r *retireRefusal
				switch boundary {
				case "done":
					_, r = retireMarkDone(t.Context(), m, j)
				case "status":
					_, r = retireStatusPostcondition(t.Context(), m, j)
				case "marker":
					_, r = retireClearMarker(t.Context(), m, root, dir, j)
				case "settlement":
					r = retireMarkSettled(t.Context(), m, &j)
				}
				why := nodeUnit + " is " + activity
				if boundary == "marker" || boundary == "settlement" {
					why = "retained-node-registration-unproved"
				}
				requireRetireProofRefusal(t, r, why)
				if j.Settled {
					t.Fatal("quiet entry changed in-memory settlement")
				}
			})
		}
	}
}

func TestRetirementSettledModesExcludeAllOtherOperations(t *testing.T) {
	for _, closing := range []bool{false, true} {
		base := retireMode{checkSettledEntry: !closing, checkSettledClosing: closing, configPath: "/etc/billet/billet.yaml",
			run: "ci-1", retiringHost: "control-a", transition: retireTestID, expectedHolder: "ci-1", expectedGuard: strings.Repeat("a", 32)}
		if r := checkRetireCombination(base); r != nil {
			t.Fatal(r)
		}
		for name, mutate := range map[string]func(*retireMode){
			"both":           func(m *retireMode) { m.checkSettledEntry, m.checkSettledClosing = true, true },
			"node-config":    func(m *retireMode) { m.checkNodeConfig = true },
			"continuation":   func(m *retireMode) { m.input = "-" },
			"reserve":        func(m *retireMode) { m.reserve = true },
			"abandon":        func(m *retireMode) { m.abandon = true },
			"complete":       func(m *retireMode) { m.completeRow = true },
			"acknowledge":    func(m *retireMode) { m.acknowledge = true },
			"classification": func(m *retireMode) { m.dryRun = true },
			"request":        func(m *retireMode) { m.requested = true },
			"server-only":    func(m *retireMode) { m.serverOnly = true },
			"survivor":       func(m *retireMode) { m.survivorHost = "control-b" },
			"completion":     func(m *retireMode) { m.completion = "-" },
			"answer":         func(m *retireMode) { m.answer = "-" },
			"config":         func(m *retireMode) { m.configPath = "" },
			"holder":         func(m *retireMode) { m.expectedHolder = "other" },
			"guard":          func(m *retireMode) { m.expectedGuard = "" },
			"transition":     func(m *retireMode) { m.transition = "" },
			"host":           func(m *retireMode) { m.retiringHost = "bad\nhost" },
		} {
			t.Run(retireSettledPurpose(base)+"/"+name, func(t *testing.T) {
				m := base
				mutate(&m)
				if r := checkRetireCombination(m); r == nil || r.Reason != retireReasonCombination {
					t.Fatalf("incompatible mode admitted: %+v", m)
				}
			})
		}
	}
}

// The TestThe prefix intentionally places producers in the t command shard.
func TestTheRetireSettledEntryFixturesAreTheCommandsOwn(t *testing.T) {
	retireSettledFixtures(t, retirement.PurposeSettledEntry,
		[]string{"active", "reenabled", "inactive", "failed", "activating", "deactivating", "reloading", "queued-job", "unknown-job", "blank-job", "unit-mismatch"})
}

func TestTheRetireSettledClosingFixturesAreTheCommandsOwn(t *testing.T) {
	retireSettledFixtures(t, retirement.PurposeSettledClosing, []string{"active", "reenabled", "inactive", "failed", "blank-job", "unit-mismatch"})
}

func retireSettledFixtures(t *testing.T, purpose string, names []string) {
	t.Helper()
	f, j := settledNodeConfigFixture(t)
	args := settledCheckArgs(t, f, j, purpose)
	family := "retire-" + purpose
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{filepath.Join(f.unitsDir, serverUnit), filepath.Join(f.unitsDir, nodeUnit), filepath.Join(f.unitsDir, nodeUnit+".effects"), registrationRecordPath} {
				before := mustRead(t, path)
				info, err := os.Stat(path)
				mustOK(t, err)
				t.Cleanup(func() { writeFile(t, path, before, info.Mode().Perm()) })
			}
			switch name {
			case "reenabled":
				f.manager.set(serverUnit, "UnitFileState", "enabled")
			case "active":
			case "inactive", "failed":
				quietSettledNode(t, f, name)
			case "queued-job":
				setRetireEffect(t, f, nodeUnit, "Job", "123 /org/freedesktop/systemd1/job/123")
			case "unknown-job":
				setRetireUnitProperty(t, filepath.Join(f.unitsDir, nodeUnit+".effects"), "Job", "")
			case "blank-job":
				setRetireEffect(t, f, nodeUnit, "Job", " ")
			case "unit-mismatch":
				setRetireEffect(t, f, nodeUnit, "NeedDaemonReload", "yes")
			default:
				f.manager.set(nodeUnit, "ActiveState", name)
			}
			forbidSettledWrites(t, f)
			out, code := f.runRaw(t, "", args...)
			if name == "active" || name == "reenabled" || purpose == retirement.PurposeSettledEntry && (name == "inactive" || name == "failed") {
				_, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, purpose))
				mustOK(t, err)
			} else {
				_, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose)
				mustOK(t, err)
			}
			compareFixture(t, family, name, normaliseReport(t, out, map[string]string{
				j.Deployment: retireTestIdentity, f.guard.record(t).ID: strings.Repeat("1", 32),
				j.Provenance.TransitionID: strings.Repeat("2", 32),
			}))
		})
	}
	fixtureSetIs(t, family, names)
}

// Calling a preparing lock helper instead of the inspection helper recreates
// missing storage; both modes must leave each absent name absent.
func TestRetirementSettledModesNeverPrepareStorage(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
		for _, path := range []string{upgradeRoot, filepath.Join(upgradeRoot, txLockName), retirement.GlobalLockPath()} {
			t.Run(purpose+"/"+filepath.Base(path), func(t *testing.T) {
				args := settledCheckArgs(t, f, j, purpose)
				mustOK(t, os.Rename(path, path+"-held"))
				t.Cleanup(func() { mustOK(t, os.Rename(path+"-held", path)) })
				out, code := f.runRaw(t, "", args...)
				refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose)
				if err != nil || code != exitUnknown || refusal.Reason != retireReasonLock {
					t.Fatalf("missing storage did not refuse at inspection lock: %s (%v)", out, err)
				}
				if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("read-only mode prepared %s: %v", path, err)
				}
			})
		}
	}
}
