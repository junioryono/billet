package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

// The fixture keeps loaded runtime properties separate from installation
// sources. Its executable returns only the properties requested by production.
func installRetireOperationEvidence(t *testing.T, f *requestFixture) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(f.unitsDir, "billet-upgrade.service"),
		"LoadState=loaded\nActiveState=inactive\nUnitFileState=static\n", 0o644)
	for _, unit := range []string{serverUnit, nodeUnit, backupServiceUnit, "billet-upgrade.service", upgradeTimerUnit, backupTimerUnit} {
		source := filepath.Join(root, unit)
		body := "[Unit]\nDescription=fixture\n"
		if unit != backupServiceUnit && unit != "billet-upgrade.service" {
			body += "[Install]\nWantedBy=multi-user.target\n"
		}
		writeFile(t, source, body, 0o644)
		properties := map[string]string{
			"Id": unit, "Names": unit, "FragmentPath": source, "SourcePath": "", "DropInPaths": "",
			"NeedDaemonReload": "no", "OnSuccessJobMode": "replace", "OnFailureJobMode": "replace",
			"FailureAction": "none", "SuccessAction": "none", "StartLimitAction": "none", "JobTimeoutAction": "none",
			"RequiresMountsFor": "", "Job": "", "ControlPID": "0", "StopWhenUnneeded": "no",
		}
		for _, key := range []string{
			"Requires", "Requisite", "Wants", "BindsTo", "Upholds", "PartOf", "RequiredBy", "RequisiteOf", "WantedBy",
			"BoundBy", "UpheldBy", "ConsistsOf", "Conflicts", "ConflictedBy", "OnSuccess", "OnFailure", "OnSuccessOf",
			"OnFailureOf", "Triggers", "TriggeredBy", "PropagatesStopTo", "StopPropagatedFrom", "JoinsNamespaceOf",
			"StateDirectory", "RuntimeDirectory", "CacheDirectory", "LogsDirectory", "ConfigurationDirectory",
			"StateDirectorySymlink", "RuntimeDirectorySymlink", "CacheDirectorySymlink", "LogsDirectorySymlink",
			"RootDirectory", "RootImage", "BindPaths", "BindReadOnlyPaths", "TemporaryFileSystem", "MountImages", "ExtensionImages", "ExtensionDirectories",
			"ExecCondition", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost",
		} {
			properties[key] = ""
		}
		properties["DynamicUser"], properties["RuntimeDirectoryPreserve"] = "no", "no"
		var lines []string
		for key, value := range properties {
			lines = append(lines, key+"="+value)
		}
		slices.Sort(lines)
		writeFile(t, filepath.Join(f.unitsDir, unit+".effects"), strings.Join(lines, "\n")+"\n", 0o644)
	}
	busctl := filepath.Join(root, "busctl")
	writeFile(t, busctl, `#!/bin/sh
set -eu
[ "$1" = get-property ] || exit 2
[ "$2" = org.freedesktop.systemd1 ] || exit 2
[ "$4" = org.freedesktop.systemd1.Service ] || exit 2
case "$3" in
  /org/freedesktop/systemd1/unit/billet_2dserver_2eservice) unit=billet-server.service ;;
  /org/freedesktop/systemd1/unit/billet_2dnode_2eservice) unit=billet-node.service ;;
  /org/freedesktop/systemd1/unit/billet_2dbackup_2eservice) unit=billet-backup.service ;;
  /org/freedesktop/systemd1/unit/billet_2dupgrade_2eservice) unit=billet-upgrade.service ;;
  *) exit 2 ;;
esac
shift 4
for property do
  case "$property" in
    StateDirectorySymlink|RuntimeDirectorySymlink|CacheDirectorySymlink|LogsDirectorySymlink) signature='a(sst)' ;;
    BindPaths|BindReadOnlyPaths) signature='a(ssbt)' ;;
    MountImages) signature='a(ssba(ss))' ;;
    ExtensionImages) signature='a(sba(ss))' ;;
    TemporaryFileSystem) signature='a(ss)' ;;
    ExecCondition|ExecStartPre|ExecStartPost|ExecStop|ExecStopPost) signature='a(sasbttttuii)' ;;
    *) exit 2 ;;
  esac
  record=$(grep "^$property=" "$BILLET_FAKE_UNITS/$unit.effects") || exit $?
  value=${record#*=}
  if [ -z "$value" ]; then
    printf '%s 0\n' "$signature"
  else
    printf '%s 1 %s\n' "$signature" "$value"
  fi
done
`, 0o755)
	saved := retireOperationInspector
	retireOperationInspector = func() *lifeops.Inspector {
		return lifeops.NewInspector(lifeops.WithSystemctl(systemctlBinary), lifeops.WithOperationUnitDirectories(root), lifeops.WithOperationBusctl(busctl))
	}
	t.Cleanup(func() { retireOperationInspector = saved })
}

func setRetireEffect(t *testing.T, f *requestFixture, unit, key, value string) {
	t.Helper()
	path := filepath.Join(f.unitsDir, unit+".effects")
	lines := strings.Split(strings.TrimSuffix(mustRead(t, path), "\n"), "\n")
	lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, key+"=") })
	lines = append(lines, key+"="+value)
	writeFile(t, path, strings.Join(lines, "\n")+"\n", 0o644)
}

func TestRetirementAdmitsTheWholeSequenceBeforeIntent(t *testing.T) {
	for _, c := range []struct{ name, unit, property, value string }{
		{"controller success", serverUnit, "OnSuccess", backupTimerUnit},
		{"controller stop propagation", serverUnit, "PropagatesStopTo", nodeUnit},
		{"upgrade timer stop", upgradeTimerUnit, "PropagatesStopTo", nodeUnit},
		{"backup timer success", backupTimerUnit, "OnSuccess", backupServiceUnit},
		{"node activation", nodeUnit, "Requires", serverUnit},
		{"reverse stop", nodeUnit, "PartOf", serverUnit},
		{"shared runtime", serverUnit, "RuntimeDirectory", "billet/registration"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			setRetireEffect(t, f, c.unit, c.property, c.value)
			if c.name == "shared runtime" {
				setRetireEffect(t, f, nodeUnit, "RuntimeDirectory", "billet/registration")
			}
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			answer := retireAnswer(t, out)
			if code != exitUnknown || answer["reason"] != retireReasonEffects {
				t.Fatalf("missing preventive effects refusal: %s", out)
			}
			if len(f.manager.operations) != 0 {
				t.Fatalf("effects refusal performed operations: %v", f.manager.operations)
			}
			if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
				t.Fatalf("effects refusal published intent: %v %v", presence, err)
			}
		})
	}
}

// Drift is introduced by a completed operation, after whole-sequence admission.
// Removing the individual helper admission must submit the next unsafe job.
func TestRetirementReadmitsAfterEachStopWait(t *testing.T) {
	for _, unit := range []string{upgradeTimerUnit, backupTimerUnit, serverUnit} {
		t.Run(unit, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
			f.svc.onStop = func(stopped string) {
				if stopped != unit {
					return
				}
				props, err := retireOperationInspector().UnitProperties(t.Context(), unit, "FragmentPath")
				mustOK(t, err)
				writeFile(t, firstProp(props, "FragmentPath"), "[Install]\nAlso="+nodeUnit+"\n", 0o644)
			}
			next, r := retireStop(t.Context(), j)
			if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "Also=") {
				t.Fatalf("stop wait did not revalidate installation: %+v", r)
			}
			if next.Phase != j.Phase || slices.Contains(f.manager.operations, "disable "+unit) ||
				!slices.Contains(f.manager.operations, "stop "+unit) {
				t.Fatalf("unsafe disable or phase advance: %s %v", next.Phase, f.manager.operations)
			}
		})
	}
}

// Each boundary is entered after successful preventive admission. The hooks
// move observations only; production chooses whether publication may proceed.
func TestRetirementReprovesEachStoppedBoundary(t *testing.T) {
	for _, boundary := range []string{"status", "journal", "archive"} {
		for _, drift := range []string{"timer", "controller process", "node invocation", "registration", "registration endpoint", "resource", "timer enablement", "controller job", "backup process"} {
			t.Run(boundary+"/"+drift, func(t *testing.T) {
				f := newRequestFixture(t)
				f.retainANode(t)
				f.reserve(t)
				j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
				mustOK(t, retirement.WriteStatus(retirement.PhaseIntent, j.Variant, retireNow()))
				if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
					t.Fatalf("healthy initial admission: %+v", r)
				}
				drifted := false
				move := func() {
					if drifted {
						return
					}
					drifted = true
					switch drift {
					case "timer":
						f.manager.set(backupTimerUnit, "ActiveState", "active")
					case "timer enablement":
						f.manager.set(backupTimerUnit, "UnitFileState", "enabled")
					case "controller job":
						setRetireEffect(t, f, serverUnit, "Job", "42")
					case "backup process":
						f.manager.set(backupServiceUnit, "LoadState", "loaded")
						f.manager.set(backupServiceUnit, "UnitFileState", "static")
						f.manager.set(backupServiceUnit, "MainPID", "42")
					case "controller process":
						f.manager.set(serverUnit, "MainPID", "42")
					case "node invocation":
						f.manager.set(nodeUnit, "InvocationID", strings.Repeat("f", 32))
					case "registration":
						mustOK(t, os.Remove(registrationRecordPath))
					case "registration endpoint":
						writeRegistrationRecord(t, registrationRecordPath, f.identity, "https://changed.example:7717", retainedInvocation)
					case "resource":
						mustOK(t, os.Chmod(filepath.Dir(registrationRecordPath), 0o755))
					}
				}
				savedSync, savedRename := retirement.SyncingDir, retireBeforeRename
				retirement.SyncingDir = func(dir string) error {
					if boundary == "journal" && dir == retirement.Root {
						status, presence, err := retirement.ReadStatus()
						mustOK(t, err)
						if presence == retirement.StatusPresent && status.Phase == retirement.PhaseStopped {
							move()
						}
					}
					return nil
				}
				retireBeforeRename = func() {
					if boundary == "archive" {
						move()
					}
				}
				t.Cleanup(func() { retirement.SyncingDir, retireBeforeRename = savedSync, savedRename })
				if boundary == "status" {
					f.svc.onStop = func(unit string) {
						if unit == serverUnit {
							move()
						}
					}
				}
				next, r := retireStop(t.Context(), j)
				if boundary == "archive" {
					if r != nil {
						t.Fatalf("healthy stopped boundaries: %+v", r)
					}
					j = next
					next, r = retireArchive(t.Context(), j)
				}
				if !drifted || r == nil || r.Reason != retireReasonStopped || next.Phase != j.Phase {
					t.Fatalf("boundary admitted drift: drift=%v next=%s refusal=%+v", drifted, next.Phase, r)
				}
				if _, err := os.Stat(j.IdentityDir); err != nil {
					t.Fatalf("boundary lost the identity: %v", err)
				}
				if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
					t.Fatalf("boundary archived the identity: %v", err)
				}
				status, presence, err := retirement.ReadStatus()
				mustOK(t, err)
				want := retirement.PhaseStopped
				if boundary == "status" {
					want = retirement.PhaseIntent
				}
				if presence != retirement.StatusPresent || status.Phase != want || requireRetireJournal(t).Phase != j.Phase {
					t.Fatalf("boundary publication advanced: status=%+v journal=%+v", status, requireRetireJournal(t))
				}
			})
		}
	}
}

func TestRetirementReadmitsChangedDefinitionsOnResume(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
		t.Fatalf("healthy first admission: %+v", r)
	}
	setRetireEffect(t, f, serverUnit, "OnSuccess", backupTimerUnit)
	before := mustRead(t, retirement.JournalPath())
	out, code := retiredRequest(t, f, requestRun)
	if code != exitUnknown || retireAnswer(t, out)["reason"] != retireReasonEffects {
		t.Fatalf("changed resume did not refuse: %s", out)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("changed resume mutated the transition: %v", f.manager.operations)
	}
}

func TestRetirementRefusesEachOperationAtItsOwnBoundary(t *testing.T) {
	operations := []lifeops.Operation{
		{Verb: "stop", Unit: upgradeTimerUnit}, {Verb: "disable", Unit: upgradeTimerUnit},
		{Verb: "stop", Unit: backupTimerUnit}, {Verb: "disable", Unit: backupTimerUnit},
		{Verb: "stop", Unit: serverUnit}, {Verb: "disable", Unit: serverUnit},
		{Verb: "enable", Unit: nodeUnit}, {Verb: "stop", Unit: nodeUnit}, {Verb: "start", Unit: nodeUnit},
	}
	for _, op := range operations {
		for _, unsafe := range []bool{false, true} {
			name := op.Verb + "/" + op.Unit + "/clean"
			if unsafe {
				name = op.Verb + "/" + op.Unit + "/unsafe"
			}
			t.Run(name, func(t *testing.T) {
				f := newRequestFixture(t)
				f.retainANode(t)
				f.reserve(t)
				phase := retirement.PhaseIntent
				if op.Unit == nodeUnit {
					phase = retirement.PhaseConfigRewritten
				}
				j := f.plantJournal(t, phase, retirement.VariantRetainedNode)
				if r := admitRetireOperations(t.Context(), j, retireServiceSequence(j, retirement.Decision{Action: retirement.ActionStop})); r != nil {
					t.Fatalf("healthy sequence: %+v", r)
				}
				poison := func() {
					if !unsafe {
						return
					}
					if op.Verb == "disable" || op.Verb == "enable" {
						props, err := retireOperationInspector().UnitProperties(t.Context(), op.Unit, "FragmentPath")
						mustOK(t, err)
						writeFile(t, firstProp(props, "FragmentPath"), "[Install]\nAlso="+backupServiceUnit+"\n", 0o644)
					} else {
						property := "PropagatesStopTo"
						if op.Verb == "start" {
							property = "Requires"
						}
						setRetireEffect(t, f, op.Unit, property, backupServiceUnit)
					}
				}
				switch {
				case op.Verb == "disable" || op.Verb == "start":
					f.svc.onStop = func(unit string) {
						if unit == op.Unit {
							poison()
						}
					}
				case op.Unit == nodeUnit && op.Verb == "stop":
					f.manager.onEnable = func(_ string) { poison() }
				default:
					poison()
				}
				var r *retireRefusal
				next := j
				if op.Unit == nodeUnit {
					next, r = retireRestartNode(t.Context(), j)
				} else {
					r = stopAndDisableForRetirement(t.Context(), f.manager, j, op.Unit)
				}
				trigger := op.Verb + " " + op.Unit
				if !unsafe {
					if r != nil || !slices.Contains(f.manager.operations, trigger) {
						t.Fatalf("clean operation refused: %+v operations=%v", r, f.manager.operations)
					}
					return
				}
				if r == nil || r.Reason != retireReasonEffects || slices.Contains(f.manager.operations, trigger) || next.Phase != j.Phase {
					t.Fatalf("unsafe triggering operation: refusal=%+v phase=%s operations=%v", r, next.Phase, f.manager.operations)
				}
				if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
					t.Fatalf("unsafe operation reached archive: %v", err)
				}
			})
		}
	}
}

func TestRetirementDoesNotInventOriginalInvocationEvidenceOnResume(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	j.RetainedInvocation = nil
	mustOK(t, j.Write(retireNow()))
	before := mustRead(t, retirement.JournalPath())
	out, code := retiredRequest(t, f, requestRun)
	answer := retireAnswer(t, out)
	if code != exitUnknown || answer["reason"] != retireReasonStopped || !strings.Contains(whyOf(answer), "no original") {
		t.Fatalf("missing original evidence was adopted: %s", out)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("missing evidence permitted mutation: %v", f.manager.operations)
	}
}

// R35: completing the archive rename is not permission to certify it after
// a blocking flush. Resume must reconcile the moved directory from this phase.
func TestRetirementReprovesArchiveAfterFlush(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	j, r := retireStop(t.Context(), j)
	if r != nil {
		t.Fatalf("healthy stop: %+v", r)
	}
	saved := retireSyncDir
	retireSyncDir = func(dir string) error {
		if dir == filepath.Dir(j.Archive) {
			f.manager.set(backupTimerUnit, "ActiveState", "active")
		}
		return nil
	}
	t.Cleanup(func() { retireSyncDir = saved })
	next, r := retireArchive(t.Context(), j)
	if r == nil || r.Reason != retireReasonStopped || next.Phase != retirement.PhaseStopped || requireRetireJournal(t).Phase != retirement.PhaseStopped {
		t.Fatalf("archive flush certified stale stopped evidence: next=%s refusal=%+v", next.Phase, r)
	}
	if _, err := os.Stat(j.Archive); err != nil {
		t.Fatalf("the completed rename was unexpectedly undone: %v", err)
	}
}

func TestRetirementReadmitsAfterConfigFileFlush(t *testing.T) {
	for _, drift := range []string{"operation effects", "invocation"} {
		t.Run(drift, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			j, r := retireStop(t.Context(), j)
			if r != nil {
				t.Fatalf("healthy stop: %+v", r)
			}
			j, r = retireArchive(t.Context(), j)
			if r != nil {
				t.Fatalf("healthy archive: %+v", r)
			}
			before := mustRead(t, f.cfg)
			saved := retireBeforeConfigRename
			retireBeforeConfigRename = func() {
				if drift == "operation effects" {
					setRetireEffect(t, f, nodeUnit, "Requires", serverUnit)
				} else {
					f.manager.set(nodeUnit, "InvocationID", strings.Repeat("f", 32))
				}
			}
			t.Cleanup(func() { retireBeforeConfigRename = saved })
			next, r := retireRewrite(t.Context(), retireProofMode(f), nil, j)
			want := retireReasonEffects
			if drift == "invocation" {
				want = retireReasonStopped
			}
			if r == nil || r.Reason != want || next.Phase != retirement.PhaseArchived || mustRead(t, f.cfg) != before {
				t.Fatalf("config replacement crossed changed evidence: phase=%s refusal=%+v", next.Phase, r)
			}
			if requireRetireJournal(t).Phase != retirement.PhaseArchived || slices.Contains(f.manager.operations, "enable "+nodeUnit) {
				t.Fatalf("failed rewrite advanced or enabled the node: %v", f.manager.operations)
			}
		})
	}
}
