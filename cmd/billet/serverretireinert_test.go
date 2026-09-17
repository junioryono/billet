package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/retirement"
)

func retireNodeServiceSuperset() []retireServiceOperation {
	return []retireServiceOperation{{Verb: "enable", Unit: nodeUnit}, {Verb: "stop", Unit: nodeUnit}, {Verb: "start", Unit: nodeUnit}}
}

// The generic request fixture has no loaded backup service. These witnesses
// require all five units loaded before capturing sources or installing drop-ins;
// positive absence otherwise legitimately skips that unit's inertness proof.
func newRetireInertFixture(t *testing.T) *requestFixture {
	t.Helper()
	f := newRequestFixture(t)
	f.manager.set(backupServiceUnit, "LoadState", "loaded")
	f.manager.set(backupServiceUnit, "UnitFileState", "static")
	for _, unit := range retireInertUnits {
		props, err := retireOperationInspector().UnitProperties(t.Context(), unit, "LoadState")
		mustOK(t, err)
		if firstProp(props, "LoadState") != "loaded" {
			t.Fatalf("inertness witness requires %s loaded: %v", unit, props)
		}
	}
	return f
}

func settledRetireInertFixture(t *testing.T) (*requestFixture, retirement.Journal) {
	t.Helper()
	return settleNodeConfigFixture(t, newRetireInertFixture(t))
}

// Plant each durable crash window, then resume through the command's guard,
// preparation and remaining-operation admission, not just the install helper.
func TestRetirementInertInstallResumesEveryCrashWindow(t *testing.T) {
	for count := 0; count <= len(retireInertUnits)+2; count++ {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			f := newRetireInertFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if count > 0 {
				record := retireInertRecord{Schema: 1, Transition: j.Provenance.TransitionID, Deployment: j.Deployment, Sources: j.InertSources}
				body, err := json.Marshal(record)
				mustOK(t, err)
				writeFile(t, retireInertMarker(j), string(body), 0o600)
			}
			for index, unit := range retireInertUnits {
				if index+1 >= count {
					break
				}
				path, err := retireInertDropIn(retireOperationInspector(), unit)
				mustOK(t, err)
				writeFile(t, path, retireInertBytes(j), 0o644)
				setRetireEffect(t, f, unit, "NeedDaemonReload", "yes")
			}
			if count == len(retireInertUnits)+2 {
				mustOK(t, reloadRetireManagerFake(f.unitsDir))
			}
			before := 0
			if body, err := os.ReadFile(filepath.Join(f.unitsDir, ".submitted")); err == nil {
				before = strings.Count(string(body), "daemon-reload")
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			out, code := retainedResume(t, f)
			retiredAnswer(t, out, code)
			calls := mustRead(t, filepath.Join(f.unitsDir, ".submitted"))
			want := 1
			if count == len(retireInertUnits)+2 {
				want = 0
			}
			if strings.Count(calls, "daemon-reload")-before != want {
				t.Fatalf("resume reloaded the wrong number of times: %s", calls)
			}
			if r := proveRetireInert(t.Context(), requireRetireJournal(t)); r != nil {
				t.Fatal(r)
			}
		})
	}
}

// A persistent mask is inertness systemd enforces itself, so the proof accepts
// it without a drop-in; a runtime mask and a mask whose fragment is a real file
// are not that, and must not pass for looking like it.
func TestRetirementInertProofAcceptsOnlyAPersistentMaskWithoutADropIn(t *testing.T) {
	mask := func(t *testing.T, f *requestFixture, shape string) {
		t.Helper()
		// A MASKED SERVER ALSO HAS TO ACCOUNT FOR ITSELF: the retired account
		// observation reads the recorded service account and looks it up, so
		// without this the case would refuse for that instead of the mask.
		account := retirement.ServiceAccount{User: "billet", UID: 123, Group: "billet", GID: 456}
		mustOK(t, retirement.WriteServiceAccount(account))
		savedUser, savedGroup := retireLookupUser, retireLookupGroup
		t.Cleanup(func() { retireLookupUser, retireLookupGroup = savedUser, savedGroup })
		retireLookupUser = func(name string) (*user.User, error) {
			return &user.User{Username: name, Uid: "123", Gid: "456", HomeDir: retirement.Root}, nil
		}
		retireLookupGroup = func(name string) (*user.Group, error) {
			return &user.Group{Name: name, Gid: "456"}, nil
		}
		path, err := retireInertDropIn(retireOperationInspector(), serverUnit)
		mustOK(t, err)
		mustOK(t, os.Remove(path))
		setRetireEffect(t, f, serverUnit, "DropInPaths", "")
		writeFile(t, filepath.Join(f.unitsDir, serverUnit+".Conditions.json"), `{"type":"a(sbbsi)","data":[]}`, 0o600)
		f.manager.set(serverUnit, "LoadState", "masked")
		f.manager.set(serverUnit, "UnitFileState", "masked")
		setRetireEffect(t, f, serverUnit, "FragmentPath", "/dev/null")
		switch shape {
		case "runtime":
			f.manager.set(serverUnit, "UnitFileState", "masked-runtime")
		case "real fragment":
			fragment := filepath.Join(f.unitsDir, serverUnit)
			setRetireEffect(t, f, serverUnit, "FragmentPath", fragment)
		case "reload":
			setRetireEffect(t, f, serverUnit, "NeedDaemonReload", "yes")
		default:
			if property, missing := strings.CutPrefix(shape, "missing "); missing {
				file := filepath.Join(f.unitsDir, serverUnit)
				if property == "FragmentPath" || property == "NeedDaemonReload" {
					file += ".effects"
				}
				setRetireUnitProperty(t, file, property, "")
			}
		}
	}
	for _, shape := range []string{"persistent", "runtime", "real fragment", "reload", "missing LoadState", "missing UnitFileState", "missing FragmentPath", "missing NeedDaemonReload"} {
		t.Run(shape, func(t *testing.T) {
			want := retireReasonInertCondition
			if shape == "reload" {
				want = retireReasonInertReload
			}
			t.Run("settled", func(t *testing.T) {
				f, j := settledRetireInertFixture(t)
				mask(t, f, shape)
				setRetireEffect(t, f, nodeUnit, "Wants", serverUnit)
				setRetireEffect(t, f, serverUnit, "WantedBy", nodeUnit)
				for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
					out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, purpose)...)
					if shape == "persistent" {
						if _, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, purpose)); err != nil {
							t.Fatalf("%s refused a persistent mask: %s (%v)", purpose, out, err)
						}
					} else if refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose); err != nil || refusal.Reason != want {
						t.Fatalf("%s missed %s: %s (%v)", purpose, shape, out, err)
					}
				}
				operations := emptyNodeOperations()
				operations.Services = retireNodeServiceSuperset()
				out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
				if shape == "persistent" {
					if code != 0 {
						t.Fatalf("node-config refused a persistent mask: %s", out)
					}
				} else if code == 0 || !strings.Contains(out, want) {
					t.Fatalf("node-config missed %s: %s", shape, out)
				}
				_, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j)
				if shape == "persistent" {
					if r != nil {
						t.Fatalf("done refused a persistent mask: %+v", r)
					}
				} else if r == nil || r.Reason != want {
					t.Fatalf("done missed %s: %+v", shape, r)
				}
			})
			t.Run("stopped", func(t *testing.T) {
				f := newRetireInertFixture(t)
				retainAndRestartANode(t, f)
				f.reserve(t)
				j := plantResumedRetirement(t, f, retirement.PhaseStopped, retirement.VariantRetainedNode)
				mask(t, f, shape)
				r := proveRetireStopped(t.Context(), j)
				if shape == "persistent" {
					if r != nil {
						t.Fatalf("stopped/archive refused a persistent mask: %+v", r)
					}
				} else if r == nil || r.Reason != want {
					t.Fatalf("stopped/archive missed %s: %+v", shape, r)
				}
			})
		})
	}
}

func TestRetirementInertProofRefusesEachBrokenConjunct(t *testing.T) {
	for _, hazard := range []string{"marker", "altered marker", "drop-in", "altered drop-in", "unloaded", "reload", "reset", "condition", "duplicate", "unknown load", "missing property"} {
		t.Run(hazard, func(t *testing.T) {
			f, j := settledNodeConfigFixture(t)
			path, err := retireInertDropIn(retireOperationInspector(), serverUnit)
			mustOK(t, err)
			want := retireReasonInertCondition
			switch hazard {
			case "marker":
				mustOK(t, os.Remove(retireInertMarker(j)))
				want = retireReasonInertMarker
			case "altered marker":
				writeFile(t, retireInertMarker(j), "{}", 0o600)
				want = retireReasonInertMarker
			case "drop-in":
				mustOK(t, os.Remove(path))
				want = retireReasonInertDropIn
			case "altered drop-in":
				writeFile(t, path, "[Unit]\n", 0o644)
				want = retireReasonInertDropIn
			case "unloaded":
				setRetireEffect(t, f, serverUnit, "DropInPaths", "")
				want = retireReasonInertDropIn
			case "reload":
				setRetireEffect(t, f, serverUnit, "NeedDaemonReload", "yes")
				want = retireReasonInertReload
			case "reset":
				writeFile(t, filepath.Join(filepath.Dir(path), "90-reset.conf"), "[Unit]\nConditionPathExists=\n", 0o644)
			case "condition":
				writeFile(t, filepath.Join(f.unitsDir, serverUnit+".Conditions.json"), `{"type":"a(sbbsi)","data":[]}`, 0o600)
			case "duplicate":
				body, err := json.Marshal(map[string]any{"type": "a(sbbsi)", "data": []any{
					[]any{"ConditionPathExists", false, true, retireInertMarker(j), 0},
					[]any{"ConditionPathExists", false, true, retireInertMarker(j), 0}}})
				mustOK(t, err)
				writeFile(t, filepath.Join(f.unitsDir, serverUnit+".Conditions.json"), string(body), 0o600)
			case "unknown load":
				f.manager.set(serverUnit, "LoadState", "future")
			case "missing property":
				setRetireUnitProperty(t, filepath.Join(f.unitsDir, serverUnit+".effects"), "NeedDaemonReload", "")
			}
			for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
				out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, purpose)...)
				refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose)
				if err != nil || refusal.Reason != want {
					t.Fatalf("%s: wanted %s: %s (%v)", purpose, want, out, err)
				}
			}
			if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r == nil || r.Reason != want {
				t.Fatalf("strict done missed %s: %+v", want, r)
			}
			if r := proveRetireStopped(t.Context(), j); r == nil || r.Reason != want {
				t.Fatalf("stopped/archive missed %s: %+v", want, r)
			}
		})
	}
}

func TestRetirementInertResumeRefusesUnrelatedReloadOrSourceChange(t *testing.T) {
	for _, hazard := range []string{"unrelated reload", "fragment", "other drop-in", "staging lookalike", "altered retirement drop-in"} {
		t.Run(hazard, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			path, err := retireInertDropIn(retireOperationInspector(), serverUnit)
			mustOK(t, err)
			want := retireReasonInertSource
			switch hazard {
			case "unrelated reload":
				setRetireEffect(t, f, nodeUnit, "NeedDaemonReload", "yes")
				want = retireReasonInertReload
			case "fragment":
				writeFile(t, filepath.Join(filepath.Dir(filepath.Dir(path)), serverUnit), "[Unit]\nDescription=changed\n", 0o644)
			case "other drop-in":
				writeFile(t, filepath.Join(filepath.Dir(path), "20-other.conf"), "[Unit]\nDescription=changed\n", 0o644)
			case "staging lookalike":
				writeFile(t, filepath.Join(filepath.Dir(path), ".durable-123.conf"), "[Unit]\nDescription=changed\n", 0o644)
			case "altered retirement drop-in":
				writeFile(t, path, "[Unit]\nDescription=other\n", 0o644)
				want = retireReasonInertInstall
			}
			out, code := retainedResume(t, f)
			if code == 0 || !strings.Contains(out, want) {
				t.Fatalf("unexpected resume: %s", out)
			}
			if body, err := os.ReadFile(filepath.Join(f.unitsDir, ".submitted")); err == nil && len(body) != 0 {
				t.Fatalf("refusal submitted service work: %s", body)
			} else if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if after := requireRetireJournal(t); after.Phase != j.Phase {
				t.Fatal("refusal advanced the journal")
			}
		})
	}
}

func TestRetirementDocumentClassesAndNodeSuperset(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	for _, kind := range []string{"metadata", "mkdir", "recursive-chown", "recursive-write", "chown"} {
		operations := emptyNodeOperations()
		operations.Filesystem = []retireFilesystemOperation{{Kind: kind, Path: retirement.Root}}
		out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
		if kind == "metadata" || kind == "mkdir" {
			if code != 0 {
				t.Fatalf("non-recursive %s refused: %s", kind, out)
			}
		} else if code == 0 || !strings.Contains(out, map[bool]string{true: retireReasonInput, false: retireReasonNodePath}[kind == "chown"]) {
			t.Fatalf("protected descendant or old class admitted: %s", out)
		}
	}
	for _, unit := range []string{nodeUnit, "billet-network.service", "billet-images-refresh.service", serverUnit} {
		operations := emptyNodeOperations()
		operations.Services = retireNodeServiceSuperset()
		for n := range operations.Services {
			operations.Services[n].Unit = unit
		}
		out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
		if (code == 0) != (unit == nodeUnit) {
			t.Fatalf("service document %s: %s", unit, out)
		}
	}
	for _, sequence := range [][]retireServiceOperation{
		{{Verb: "start", Unit: nodeUnit}},
		{{Verb: "stop", Unit: nodeUnit}, {Verb: "enable", Unit: nodeUnit}, {Verb: "start", Unit: nodeUnit}},
		{{Verb: "enable", Unit: nodeUnit}, {Verb: "stop", Unit: nodeUnit}, {Verb: "restart", Unit: nodeUnit}},
	} {
		operations := emptyNodeOperations()
		operations.Services = slices.Clone(sequence)
		out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
		if code == 0 || !strings.Contains(out, retireReasonEffects) {
			t.Fatalf("unsupported sequence admitted: %s", out)
		}
	}
}

func TestRetirementInstallsInertnessBeforeItsFirstStop(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	previous := retireMutationEvent
	var files []string
	reloaded, stopped := false, false
	retireMutationEvent = func(event, path string) {
		switch event {
		case "inert-file":
			if len(files) == 0 && path != retireInertMarker(j) {
				t.Fatal("the first durable install was not the protected marker")
			}
			if len(files) > 0 {
				if _, err := readRetireInertRecord(j); err != nil {
					t.Fatal(err)
				}
			}
			files = append(files, path)
		case "service-operation":
			if path == "daemon-reload" {
				if len(files) != 6 {
					t.Fatalf("reload preceded the marker and five drop-ins: %v", files)
				}
				reloaded = true
			} else if strings.HasPrefix(path, "stop ") || strings.HasPrefix(path, "disable ") {
				if !reloaded {
					t.Fatal("service stop/disable preceded reload")
				}
				if r := proveRetireInert(t.Context(), j); r != nil {
					t.Fatal(r)
				}
				stopped = true
			}
		}
	}
	t.Cleanup(func() { retireMutationEvent = previous })
	out, code := retainedResume(t, f)
	retiredAnswer(t, out, code)
	if !stopped {
		t.Fatal("witness never reached the stop step")
	}
}

func TestRetirementRepairsMissingOwnedFilesButNeverOtherPendingReloads(t *testing.T) {
	for _, lost := range []string{"marker", serverUnit, upgradeTimerUnit} {
		t.Run(lost, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if r := reconcileRetireInert(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatal(r)
			}
			path := retireInertMarker(j)
			if lost != "marker" {
				var err error
				path, err = retireInertDropIn(retireOperationInspector(), lost)
				mustOK(t, err)
				setRetireEffect(t, f, lost, "NeedDaemonReload", "yes")
			}
			mustOK(t, os.Remove(path))
			out, code := retainedResume(t, f)
			retiredAnswer(t, out, code)
		})
	}
}

func TestRetirementAbsentUnitStillReceivesItsDropIn(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	path, err := retireInertDropIn(retireOperationInspector(), upgradeServiceUnit)
	mustOK(t, err)
	f.manager.set(upgradeServiceUnit, "LoadState", "not-found")
	f.manager.set(upgradeServiceUnit, "UnitFileState", "")
	setRetireEffect(t, f, upgradeServiceUnit, "FragmentPath", "")
	// The candidate source remains on disk; positive loaded absence does not
	// waive installing its owned condition before a later load.
	if r := reconcileRetireInert(t.Context(), retireProofMode(f), j); r != nil {
		t.Fatal(r)
	}
	if body := mustRead(t, path); body != retireInertBytes(j) {
		t.Fatalf("absent unit lost its durable drop-in: %q", body)
	}
}

func TestRetirementSettledEnablementIsVisibleAndPublicationRemainsStrict(t *testing.T) {
	f, j := settledRetireInertFixture(t)
	want := make(map[string]string)
	for _, unit := range retireInertUnits {
		f.manager.set(unit, "UnitFileState", "enabled")
		want[unit] = "enabled"
	}
	// The installed Also relation is allowed only on the ordinary route.
	props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "FragmentPath")
	mustOK(t, err)
	path := firstProp(props, "FragmentPath")
	writeFile(t, path, mustRead(t, path)+"Also="+serverUnit+"\n", 0o644)
	for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
		out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, purpose)...)
		verdict, err := retirement.DecodeSettledVerdict([]byte(out), code, settledExpectation(t, f, j, purpose))
		if err != nil || !reflect.DeepEqual(verdict.EnablementChanges, want) {
			t.Fatalf("enabled inert units were not admitted and reported: %s (%v)", out, err)
		}
	}
	if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r == nil || r.Reason != retireReasonPostcondition {
		t.Fatalf("ordinary enablement permission leaked into publication: %+v", r)
	}
}

func TestRetirementUpgradeStaticRequiresItsQuietTimerAndNoProcess(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r != nil {
		t.Fatal(r)
	}
	for _, hazard := range []string{"timer", "process", "active"} {
		t.Run(hazard, func(t *testing.T) {
			unit, property, value := upgradeServiceUnit, "MainPID", "99"
			switch hazard {
			case "timer":
				unit, property, value = upgradeTimerUnit, "ActiveState", "active"
			case "active":
				property, value = "ActiveState", "active"
			}
			file := filepath.Join(f.unitsDir, unit)
			before := mustRead(t, file)
			t.Cleanup(func() { writeFile(t, file, before, 0o644) })
			f.manager.set(unit, property, value)
			if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r == nil || r.Reason != retireReasonPostcondition {
				t.Fatalf("static upgrade incorrectly quiet with %s: %+v", hazard, r)
			}
		})
	}
}

func TestRetirementConcretePathsRefuseArchivedKeyringsAndSymlinkedAncestors(t *testing.T) {
	f, j := settledNodeConfigFixture(t)
	outside := t.TempDir()
	link := filepath.Join(outside, "archive")
	mustOK(t, os.Symlink(j.Archive, link))
	for _, kind := range []string{"write", "metadata", "recursive-chown", "recursive-write"} {
		for _, path := range []string{filepath.Join(j.Archive, "ceph.keyring"), filepath.Join(link, "ceph.keyring")} {
			operations := emptyNodeOperations()
			operations.Filesystem = []retireFilesystemOperation{{Kind: kind, Path: path}}
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
			if code == 0 || !strings.Contains(out, retireReasonNodePath) {
				t.Fatalf("%s reached archived authority through %s: %s", kind, path, out)
			}
		}
	}
	operations := emptyNodeOperations()
	operations.Filesystem = []retireFilesystemOperation{{Kind: "recursive-write", Path: filepath.Join(outside, "stage")}}
	out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
	if code != 0 {
		t.Fatalf("disjoint archive extraction refused: %s", out)
	}
	operations.Filesystem = []retireFilesystemOperation{{Kind: "metadata", Path: upgradeRoot}}
	out, code = runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
	if code == 0 || !strings.Contains(out, retireReasonNodePath) {
		t.Fatalf("upgrade root lost protection: %s", out)
	}
}

func TestRetirementEveryProtectedUnitNeedsItsDropInAtEveryProof(t *testing.T) {
	t.Run("stopped/archive", func(t *testing.T) {
		f := newRetireInertFixture(t)
		retainAndRestartANode(t, f)
		f.reserve(t)
		j := plantResumedRetirement(t, f, retirement.PhaseStopped, retirement.VariantRetainedNode)
		// Stopped/archive still binds the original invocation and config inode.
		// A settled host has already replaced both during its handoff.
		if r := proveRetireStopped(t.Context(), j); r != nil {
			t.Fatalf("healthy stopped/archive baseline: %+v", r)
		}
		for _, unit := range retireInertUnits {
			t.Run(unit, func(t *testing.T) {
				path, err := retireInertDropIn(retireOperationInspector(), unit)
				mustOK(t, err)
				body := mustRead(t, path)
				mustOK(t, os.Remove(path))
				t.Cleanup(func() { writeFile(t, path, body, 0o644) })
				if r := proveRetireStopped(t.Context(), j); r == nil || r.Reason != retireReasonInertDropIn {
					t.Fatalf("stopped/archive omitted %s: %+v", unit, r)
				}
			})
		}
	})
	f, j := settledRetireInertFixture(t)
	if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r != nil {
		t.Fatalf("healthy done baseline: %+v", r)
	}
	for _, unit := range retireInertUnits {
		t.Run(unit, func(t *testing.T) {
			path, err := retireInertDropIn(retireOperationInspector(), unit)
			mustOK(t, err)
			body := mustRead(t, path)
			mustOK(t, os.Remove(path))
			t.Cleanup(func() { writeFile(t, path, body, 0o644) })
			if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r == nil || r.Reason != retireReasonInertDropIn {
				t.Fatalf("done omitted %s: %+v", unit, r)
			}
			for _, purpose := range []string{retirement.PurposeSettledEntry, retirement.PurposeSettledClosing} {
				out, code := f.runRaw(t, "", settledCheckArgs(t, f, j, purpose)...)
				refusal, err := retirement.DecodeSettledRefusal([]byte(out), code, purpose)
				if err != nil || refusal.Reason != retireReasonInertDropIn {
					t.Fatalf("%s omitted %s: %s (%v)", purpose, unit, out, err)
				}
			}
		})
	}
}

func TestRetirementInertServicesCannotHideDescendantProcesses(t *testing.T) {
	f, j := settledRetireInertFixture(t)
	if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r != nil {
		t.Fatalf("healthy process-free baseline: %+v", r)
	}
	for _, unit := range []string{serverUnit, backupServiceUnit, upgradeServiceUnit} {
		t.Run(unit, func(t *testing.T) {
			root := retireOperationInspector().OperationUnitDirectories()[0]
			group := "/system.slice/" + unit
			setRetireEffect(t, f, unit, "ControlGroup", group)
			path := filepath.Join(root, "system.slice", unit, "cgroup.procs")
			writeFile(t, path, "123\n", 0o644)
			t.Cleanup(func() {
				writeFile(t, path, "", 0o644)
				setRetireEffect(t, f, unit, "ControlGroup", "")
			})
			if _, r := observeRetirePostconditions(t.Context(), retireMode{configPath: f.cfg}, j); r == nil || r.Reason != retireReasonInertProcess {
				t.Fatalf("descendant process not refused: %+v", r)
			}
		})
	}
}

// Removing the full intent admission lets the marker write trigger this newly
// loaded path unit even though none of the captured source files changed.
func TestRetirementIntentResumeChecksLateActivationBeforeMarker(t *testing.T) {
	for _, relation := range []string{"TriggeredBy", "OnSuccessOf"} {
		t.Run(relation, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			installRetirePathWatcher(t, f, serverUnit)
			watcherSource := filepath.Join(f.unitsDir, "watcher-source")
			writeFile(t, watcherSource, "[Path]\nPathChanged="+retirement.RetiredDir()+"\nUnit="+serverUnit+"\n", 0o644)
			setRetireEffect(t, f, "billet-backup.path", "FragmentPath", watcherSource)
			setRetireEffect(t, f, "billet-backup.path", "Triggers", serverUnit)
			if relation != "TriggeredBy" {
				setRetireEffect(t, f, serverUnit, "TriggeredBy", "")
				setRetireEffect(t, f, serverUnit, relation, "billet-backup.path")
				setRetireEffect(t, f, "billet-backup.path", "Triggers", "helper.service")
				setRetireEffect(t, f, "billet-backup.path", "OnSuccess", serverUnit)
				writeFile(t, watcherSource, "[Unit]\nOnSuccess="+serverUnit+"\n[Path]\nPathChanged="+retirement.RetiredDir()+"\nUnit=helper.service\n", 0o644)
			}
			sources, err := retireInertSources(t.Context(), retireOperationInspector())
			mustOK(t, err)
			if !reflect.DeepEqual(sources, j.InertSources) {
				t.Fatal("late loaded edge changed source evidence")
			}
			pending, err := retireOperationInspector().PendingReloadUnits(t.Context())
			mustOK(t, err)
			if len(pending) != 0 {
				t.Fatal("late edge left a pending reload")
			}
			out, code := retainedResume(t, f)
			if code == 0 || !strings.Contains(out, "operation-edge-outside-set") || !strings.Contains(out, "billet-backup.path") {
				t.Fatalf("late activation was not refused by effects admission: %s", out)
			}
			if _, err := os.Lstat(retireInertMarker(j)); !os.IsNotExist(err) {
				t.Fatalf("refusal wrote a marker or absence is unknown: %v", err)
			}
			for _, unit := range retireInertUnits {
				path, err := retireInertDropIn(retireOperationInspector(), unit)
				mustOK(t, err)
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("refusal wrote a drop-in or absence is unknown: %v", err)
				}
			}
		})
	}
}

func TestRetirementResumesInsideDurableDropInInstall(t *testing.T) {
	if path := os.Getenv("BILLET_TEST_INERT_STAGING_PATH"); path != "" {
		installer := durablefile.Installer{Rename: func(_, _ string) error {
			os.Exit(77) // Interrupt the primitive before rename and deferred cleanup.
			return nil
		}}
		_, err := installer.Install(filepath.Dir(path), filepath.Base(path), 0o644, func(w io.Writer) error {
			_, err := io.WriteString(w, "interrupted staged bytes\n")
			return err
		})
		t.Fatalf("child did not interrupt inside Install: %v", err)
	}
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	path, err := retireInertDropIn(retireOperationInspector(), serverUnit)
	mustOK(t, err)
	mustOK(t, os.MkdirAll(filepath.Dir(path), 0o755))
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRetirementResumesInsideDurableDropInInstall$", "-test.timeout=30s")
	child.Env = append(os.Environ(), "BILLET_TEST_INERT_STAGING_PATH="+path)
	out, err := child.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 77 {
		t.Fatalf("child did not reach the install interruption: %s (%v)", out, err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	mustOK(t, err)
	if len(entries) != 1 || !durablefile.IsStagingName(entries[0].Name()) {
		t.Fatalf("interruption left no recognized staging artifact: %v", entries)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("interruption already installed drop-in: %v", err)
	}
	answer, code := retainedResume(t, f)
	retiredAnswer(t, answer, code)
	if body := mustRead(t, path); body != retireInertBytes(j) {
		t.Fatal("resume did not repair the missing owned drop-in")
	}
}

func TestRetirementResumeCommitsMarkerRenameBeforeDropIns(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	markerDir := filepath.Dir(retireInertMarker(j))
	mustOK(t, os.MkdirAll(markerDir, 0o755))
	body, err := json.Marshal(retireInertRecord{Schema: 1, Transition: j.Provenance.TransitionID, Deployment: j.Deployment, Sources: j.InertSources})
	mustOK(t, err)
	interrupted := errors.New("marker directory sync interrupted")
	installer := durablefile.Installer{SyncDir: func(string) error { return interrupted }}
	_, err = installer.Install(markerDir, filepath.Base(retireInertMarker(j)), 0o600, func(w io.Writer) error {
		_, err := w.Write(body)
		return err
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("did not interrupt after marker rename: %v", err)
	}
	_, err = readRetireInertRecord(j)
	mustOK(t, err)
	previous := retireInertInstaller
	previousEvent := retireMutationEvent
	t.Cleanup(func() { retireInertInstaller, retireMutationEvent = previous, previousEvent })
	flushed := false
	fail := true
	retireInertInstaller.SyncDir = func(dir string) error {
		if dir == markerDir && fail {
			return interrupted
		}
		if err := (durablefile.Installer{}).SyncDirectory(dir); err != nil {
			return err
		}
		if dir == markerDir {
			flushed = true
		}
		return nil
	}
	dropins := 0
	retireMutationEvent = func(event, path string) {
		if event == "inert-file" && path != retireInertMarker(j) {
			if !flushed {
				t.Fatal("drop-in preceded the resumed marker directory flush")
			}
			dropins++
		}
	}
	out, code := retainedResume(t, f)
	if code == 0 || !strings.Contains(out, interrupted.Error()) || dropins != 0 {
		t.Fatalf("failed marker flush did not hold drop-ins: %s", out)
	}
	fail = false
	out, code = retainedResume(t, f)
	retiredAnswer(t, out, code)
	if !flushed || dropins != len(retireInertUnits) {
		t.Fatal("resume never committed the marker and installed all drop-ins")
	}
}

func TestRetirementLoadedInstallStillCommitsDropInDirectories(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	if r := reconcileRetireInert(t.Context(), retireProofMode(f), j); r != nil {
		t.Fatal(r)
	}
	path, err := retireInertDropIn(retireOperationInspector(), serverUnit)
	mustOK(t, err)
	previous := retireInertInstaller
	t.Cleanup(func() { retireInertInstaller = previous })
	retireInertInstaller.SyncDir = func(dir string) error {
		if dir == filepath.Dir(path) {
			return errors.New("drop-in directory sync interrupted")
		}
		return (durablefile.Installer{}).SyncDirectory(dir)
	}
	out, code := retainedResume(t, f)
	if code == 0 || !strings.Contains(out, "drop-in directory sync interrupted") {
		t.Fatalf("complete-install fast path skipped its directory flush: %s", out)
	}
	calls := mustRead(t, filepath.Join(f.unitsDir, ".submitted"))
	if strings.Contains(calls, "stop ") || strings.Contains(calls, "disable ") {
		t.Fatalf("uncommitted drop-ins reached service operations: %s", calls)
	}
}

func TestRetirementOrdinaryAlsoExaminesInertUnitInstallationTransitively(t *testing.T) {
	for _, helper := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "transitive helper"}[helper], func(t *testing.T) {
			f, j := settledNodeConfigFixture(t)
			for _, unit := range []string{nodeUnit, serverUnit} {
				props, err := retireOperationInspector().UnitProperties(t.Context(), unit, "FragmentPath")
				mustOK(t, err)
				path := firstProp(props, "FragmentPath")
				if unit == nodeUnit {
					writeFile(t, path, mustRead(t, path)+"\n[Install]\nAlso="+serverUnit+"\n", 0o644)
				} else if helper {
					writeFile(t, path, mustRead(t, path)+"\n[Install]\nAlso=helper.service\n", 0o644)
				}
			}
			operations := emptyNodeOperations()
			operations.Services = retireNodeServiceSuperset()
			out, code := runNodeConfigCheck(t, f, j, nodeConfigDocument(t, j, mustRead(t, f.cfg), operations))
			if helper {
				if code == 0 || !strings.Contains(out, "operation-edge-outside-set") || !strings.Contains(out, "Also=helper.service") {
					t.Fatalf("transitive Also escaped inspection: %s", out)
				}
			} else if code != 0 {
				t.Fatalf("clean Also refused: %s", out)
			}
		})
	}
}
