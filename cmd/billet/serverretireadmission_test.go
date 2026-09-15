package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
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
	for _, unit := range []string{serverUnit, nodeUnit, backupServiceUnit, "billet-upgrade.service", upgradeTimerUnit, backupTimerUnit, "billet-network.service", "billet-dnsmasq@br0.service", "billet-dnsmasq@br1.service", "sysinit.target", "local-fs.target", "multi-user.target", "systemd-firstboot.service", "helper.service"} {
		if unit == "helper.service" || unit == "systemd-firstboot.service" {
			writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=loaded\nActiveState=inactive\nUnitFileState=static\n", 0o644)
		}
		if strings.HasSuffix(unit, ".target") {
			writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=loaded\nActiveState=active\nUnitFileState=static\n", 0o644)
		}
		if strings.HasPrefix(unit, "billet-dnsmasq@") || unit == "billet-network.service" {
			pid := "4250"
			if unit == "billet-network.service" {
				pid = "0"
			}
			writeFile(t, filepath.Join(f.unitsDir, unit), "LoadState=loaded\nActiveState=active\nUnitFileState=enabled\nMainPID="+pid+"\nInvocationID="+retainedInvocation+"\nKillMode=control-group\n", 0o644)
		}
		source := filepath.Join(root, unit)
		body := "[Unit]\nDescription=fixture\n"
		if strings.HasSuffix(unit, ".timer") {
			body += "[Install]\nWantedBy=timers.target\n"
		} else if unit != backupServiceUnit && unit != "billet-upgrade.service" {
			body += "[Install]\nWantedBy=multi-user.target\n"
		}
		writeFile(t, source, body, 0o644)
		properties := map[string]string{
			"Id": unit, "Names": unit, "FragmentPath": source, "SourcePath": "", "DropInPaths": "",
			"StandardInput": "null", "StandardOutput": "journal", "StandardError": "inherit",
			"Transient": "no", "NeedDaemonReload": "no", "OnSuccessJobMode": "fail", "OnFailureJobMode": "replace",
			"FailureAction": "none", "SuccessAction": "none", "StartLimitAction": "none", "JobTimeoutAction": "none",
			"RequiresMountsFor": "", "Job": "", "ControlPID": "0", "ControlGroup": "", "Slice": "system.slice", "StopWhenUnneeded": "no",
		}
		for _, key := range []string{
			"Before", "After", "PropagatesReloadTo", "ReloadPropagatedFrom", "SliceOf", "Following",
			"PIDFile", "PAMName", "LogNamespace", "NetworkNamespacePath", "IPCNamespacePath", "UtmpIdentifier",
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
	setRetireEffect(t, f, "sysinit.target", "Wants", "local-fs.target systemd-firstboot.service")
	setRetireEffect(t, f, "systemd-firstboot.service", "StandardInput", "tty")
	setRetireEffect(t, f, "systemd-firstboot.service", "StandardOutput", "tty")
	setRetireEffect(t, f, "systemd-firstboot.service", "ImportCredential", "firstboot.*")
	setRetireEffect(t, f, "local-fs.target", "OnFailure", "emergency.target")
	setRetireEffect(t, f, "local-fs.target", "OnFailureJobMode", "replace-irreversibly")
	busctl := filepath.Join(root, "busctl")
	writeFile(t, busctl, `#!/bin/sh
set -eu
[ "$1" = get-property ] || exit 2
[ "$2" = org.freedesktop.systemd1 ] || exit 2
[ "$4" = org.freedesktop.systemd1.Service ] || exit 2
case "$3" in
  /org/freedesktop/systemd1/unit/helper_2eservice) unit=helper.service ;;
  /org/freedesktop/systemd1/unit/billet_2dserver_2eservice) unit=billet-server.service ;;
  /org/freedesktop/systemd1/unit/billet_2dnode_2eservice) unit=billet-node.service ;;
  /org/freedesktop/systemd1/unit/billet_2dbackup_2eservice) unit=billet-backup.service ;;
  /org/freedesktop/systemd1/unit/billet_2dupgrade_2eservice) unit=billet-upgrade.service ;;
  /org/freedesktop/systemd1/unit/billet_2dnetwork_2eservice) unit=billet-network.service ;;
  /org/freedesktop/systemd1/unit/billet_2ddnsmasq_40br0_2eservice) unit=billet-dnsmasq@br0.service ;;
  /org/freedesktop/systemd1/unit/billet_2ddnsmasq_40br1_2eservice) unit=billet-dnsmasq@br1.service ;;
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
		return lifeops.NewInspector(lifeops.WithSystemctl(systemctlBinary), lifeops.WithOperationUnitDirectories(root), lifeops.WithOperationBusctl(busctl), lifeops.WithOperationCgroupRoot(root))
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
		for _, drift := range []string{"timer", "controller process", "controller cgroup", "network activity", "network invocation", "network resource", "node invocation", "registration", "registration endpoint", "resource", "timer enablement", "controller job", "backup process"} {
			t.Run(boundary+"/"+drift, func(t *testing.T) {
				f := newRequestFixture(t)
				f.retainANode(t)
				networkResource := ""
				if strings.HasPrefix(drift, "network ") {
					f.originalNode.Provider = string(config.ProviderFirecracker)
					for _, unit := range []string{"billet-network.service", "billet-dnsmasq@br0.service"} {
						service, err := observeRetireService(t.Context(), unit)
						mustOK(t, err)
						f.originalNode.Services = append(f.originalNode.Services, service)
					}
					networkResource = t.TempDir()
					resource, err := observeRetireResource(networkResource)
					mustOK(t, err)
					resource.GuestNetwork = true
					f.originalNode.Resources = append(f.originalNode.Resources, resource)
				}
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
					case "network activity":
						f.manager.set("billet-dnsmasq@br0.service", "ActiveState", "inactive")
					case "network invocation":
						f.manager.set("billet-network.service", "InvocationID", strings.Repeat("f", 32))
					case "network resource":
						mustOK(t, os.Chmod(networkResource, 0o755))
					case "controller cgroup":
						props, err := retireOperationInspector().UnitProperties(t.Context(), serverUnit, "FragmentPath")
						mustOK(t, err)
						group := filepath.Join(filepath.Dir(firstProp(props, "FragmentPath")), "system.slice", serverUnit)
						mustOK(t, os.MkdirAll(group, 0o755))
						writeFile(t, filepath.Join(group, "cgroup.procs"), "4243\n", 0o644)
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
				savedSync, savedRename, savedProof := retirement.SyncingDir, retireBeforeRename, retireBeforeStoppedProof
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
					if boundary == "archive" && drift != "controller job" {
						move()
					}
				}
				retireBeforeStoppedProof = func() {
					if boundary == "status" && drift == "backup process" {
						move()
					}
					if boundary == "archive" && drift == "controller job" && requireRetireJournal(t).Phase == retirement.PhaseStopped {
						move()
					}
				}
				t.Cleanup(func() {
					retirement.SyncingDir, retireBeforeRename, retireBeforeStoppedProof = savedSync, savedRename, savedProof
				})
				if boundary == "status" {
					f.manager.onDisable = func(unit string) {
						if unit == serverUnit && drift != "backup process" {
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
					next, r = retireRestartNode(t.Context(), f.cfg, j)
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

func TestRetirementRechecksTerminationAtControllerAndNodeStops(t *testing.T) {
	for _, unit := range []string{serverUnit, nodeUnit} {
		for _, mode := range []string{"process", "none", ""} {
			t.Run(unit+"/"+mode, func(t *testing.T) {
				f := newRequestFixture(t)
				f.retainANode(t)
				f.reserve(t)
				phase := retirement.PhaseIntent
				if unit == nodeUnit {
					phase = retirement.PhaseConfigRewritten
				}
				j := f.plantJournal(t, phase, retirement.VariantRetainedNode)
				if r := admitRetireOperation(t.Context(), j, "stop", unit); r != nil {
					t.Fatalf("clean current policy: %+v", r)
				}
				poison := func() { f.manager.set(unit, "KillMode", mode) }
				var r *retireRefusal
				next := j
				if unit == nodeUnit {
					f.manager.onEnable = func(_ string) { poison() }
					next, r = retireRestartNode(t.Context(), f.cfg, j)
				} else {
					poison()
					r = stopAndDisableForRetirement(t.Context(), f.manager, j, unit)
				}
				if r == nil || r.Reason != retireReasonEffects || next.Phase != j.Phase || slices.Contains(f.manager.operations, "stop "+unit) {
					t.Fatalf("stale termination policy authorized stop: refusal=%+v phase=%s operations=%v", r, next.Phase, f.manager.operations)
				}
			})
		}
	}
}

func TestRetirementCapturesAndProtectsRequiredGuestNetwork(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	installed, r := observeRetireConfig(f.cfg)
	if r != nil {
		t.Fatal(r)
	}
	installed.cfg.Node.Provider = config.ProviderFirecracker
	installed.cfg.Node.Firecracker = &config.FirecrackerConfig{Bridge: "br0", UntrustedBridge: "br1"}
	original, r := captureRetireInvocation(t.Context(), installed.cfg)
	if r != nil {
		t.Fatalf("clean guest network: %+v", r)
	}
	if len(original.Services) != 3 || original.Services[0].Unit != "billet-network.service" ||
		original.Services[1].Unit != "billet-dnsmasq@br0.service" || original.Services[2].Unit != "billet-dnsmasq@br1.service" {
		t.Fatalf("installed bridges did not determine required services: %+v", original.Services)
	}
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	j.RetainedInvocation = original
	if r := admitRetireOperation(t.Context(), j, "stop", serverUnit); r != nil {
		t.Fatalf("clean controller stop: %+v", r)
	}
	setRetireEffect(t, f, serverUnit, "PropagatesStopTo", "billet-dnsmasq@br0.service")
	if r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit); r == nil || r.Reason != retireReasonEffects {
		t.Fatalf("DNS collateral not refused: %+v", r)
	}
	if slices.Contains(f.manager.operations, "stop "+serverUnit) {
		t.Fatalf("controller stopped before DNS refusal: %v", f.manager.operations)
	}
	setRetireEffect(t, f, serverUnit, "PropagatesStopTo", "")
	for _, service := range original.Services {
		f.manager.set(service.Unit, "InvocationID", strings.Repeat("f", 32))
		if r := proveRetireInvocation(t.Context(), original); r == nil || r.Reason != retireReasonStopped {
			t.Fatalf("changed network invocation admitted: %s %+v", service.Unit, r)
		}
		f.manager.set(service.Unit, "InvocationID", service.InvocationID)
	}
	installed.cfg.Node.Firecracker.Bridge = ""
	if _, r := captureRetireInvocation(t.Context(), installed.cfg); r == nil {
		t.Fatal("unknown configured guest bridges admitted")
	}
}

func TestRetirementBindsNodeStateAliasBeforeEachOperation(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	installed, r := observeRetireConfig(f.cfg)
	if r != nil {
		t.Fatal(r)
	}
	alias := filepath.Join(t.TempDir(), "node-state")
	mustOK(t, os.Symlink("/run/shared", alias))
	installed.cfg.Node.StateDir = alias
	original, r := captureRetireInvocation(t.Context(), installed.cfg)
	if r != nil {
		t.Fatalf("capture state alias: %+v", r)
	}
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	j.RetainedInvocation = original
	setRetireEffect(t, f, serverUnit, "RuntimeDirectory", "shared")
	if r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit); r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "operation-directory-overlap") {
		t.Fatalf("controller could remove aliased node.state_dir: %+v", r)
	}
	setRetireEffect(t, f, serverUnit, "RuntimeDirectory", "unrelated")
	if r := admitRetireOperation(t.Context(), j, "stop", serverUnit); r != nil {
		t.Fatalf("clean bound state alias: %+v", r)
	}
	mustOK(t, os.Remove(alias))
	mustOK(t, os.Symlink("/run/another-state", alias))
	if r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit); r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "resolution changed") {
		t.Fatalf("retargeted historical binding accepted: %+v", r)
	}
	if slices.Contains(f.manager.operations, "stop "+serverUnit) {
		t.Fatalf("path refusal submitted controller stop: %v", f.manager.operations)
	}
}

// Both execution readers receive independently encoded loaded command evidence.
func installRetireNodeExecution(t *testing.T, f *requestFixture) {
	t.Helper()
	binary, err := os.Executable()
	mustOK(t, err)
	savedBinary, savedBus := installedBinary, busctlBinary
	installedBinary = binary
	busctlBinary = filepath.Join(t.TempDir(), "busctl")
	writeFile(t, busctlBinary, `#!/bin/sh
set -eu
[ "$1" = --json=short ] || exit 2
[ "$2" = get-property ] || exit 2
[ "$3" = org.freedesktop.systemd1 ] || exit 2
[ "$4" = /org/freedesktop/systemd1/unit/billet_2dnode_2eservice ] || exit 2
[ "$5" = org.freedesktop.systemd1.Service ] || exit 2
[ "$6" = ExecStart ] || exit 2
cat "$BILLET_FAKE_UNITS/node-exec.json"
`, 0o755)
	t.Cleanup(func() { installedBinary, busctlBinary = savedBinary, savedBus })
	setRetireNodeCommand(t, f, []string{binary, "node", "--config", f.cfg}, "")
	setRetireEffect(t, f, nodeUnit, "Requires", "sysinit.target")
	setRetireEffect(t, f, nodeUnit, "After", "sysinit.target")
	for _, name := range []string{"EnvironmentFiles", "Environment"} {
		setRetireEffect(t, f, nodeUnit, name, "")
	}
}

func setRetireNodeCommand(t *testing.T, f *requestFixture, argv []string, flags string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"type": "a(sasbttttuii)", "data": []any{
		[]any{argv[0], argv, false, 0, 0, 0, 0, 0, 0, 0},
	}})
	mustOK(t, err)
	writeFile(t, filepath.Join(f.unitsDir, "node-exec.json"), string(body), 0o644)
	command := "{ path=" + argv[0] + " ; argv[]=" + strings.Join(argv, " ")
	setRetireEffect(t, f, nodeUnit, "ExecStart", command+" ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }")
	setRetireEffect(t, f, nodeUnit, "ExecStartEx", command+" ; flags="+flags+" ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }")
}

func installRetirePathWatcher(t *testing.T, f *requestFixture, unit string) {
	t.Helper()
	watcher := "billet-backup.path"
	writeFile(t, filepath.Join(f.unitsDir, watcher), "LoadState=loaded\nActiveState=inactive\n", 0o644)
	writeFile(t, filepath.Join(f.unitsDir, watcher+".effects"), mustRead(t, filepath.Join(f.unitsDir, "helper.service.effects")), 0o644)
	setRetireEffect(t, f, watcher, "Id", watcher)
	setRetireEffect(t, f, watcher, "Names", watcher)
	// Same PathChanged/Unit relationship as /var/lib/billet/server in the
	// real-systemd counterfactual, with this host's identity path substituted.
	writeFile(t, filepath.Join(f.unitsDir, "watcher-source"),
		"[Path]\nPathChanged="+f.stateDir+"\nUnit="+unit+"\n", 0o644)
	setRetireEffect(t, f, unit, "TriggeredBy", watcher)
}

func TestRetirementRefusesPathActivationBeforeArchiveRename(t *testing.T) {
	for _, chain := range []string{"direct"} {
		for _, variant := range []retirement.Variant{retirement.VariantServerOnly, retirement.VariantRetainedNode} {
			t.Run(chain+"/"+string(variant), func(t *testing.T) {
				f := newRequestFixture(t)
				if variant == retirement.VariantRetainedNode {
					f.retainANode(t)
				}
				f.reserve(t)
				j := f.plantJournal(t, retirement.PhaseIntent, variant)
				destination := backupServiceUnit
				j, r := retireStop(t.Context(), j)
				if r != nil {
					t.Fatalf("inactive path control: %+v", r)
				}
				before, err := os.Stat(j.IdentityDir)
				mustOK(t, err)
				beforeJournal := mustRead(t, retirement.JournalPath())
				saved := retireBeforeRename
				retireBeforeRename = func() { installRetirePathWatcher(t, f, destination) }
				t.Cleanup(func() { retireBeforeRename = saved })
				next, r := retireArchive(t.Context(), j)
				if r == nil || !strings.Contains(r.Why, "operation-edge-outside-set") || next.Phase != retirement.PhaseStopped {
					t.Fatalf("archive admitted an active path watcher: phase=%s refusal=%+v", next.Phase, r)
				}
				after, err := os.Stat(j.IdentityDir)
				mustOK(t, err)
				if !os.SameFile(before, after) || mustRead(t, retirement.JournalPath()) != beforeJournal {
					t.Fatal("watcher refusal changed identity or phase")
				}
				if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
					t.Fatalf("watcher refusal came after rename: %v", err)
				}
			})
		}
	}
}

func TestRetirementReprovesActivationAtStoppedPublications(t *testing.T) {
	for _, boundary := range []string{"status", "journal"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
			mustOK(t, retirement.WriteStatus(j.Phase, j.Variant, retireNow()))
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy earlier admission: %+v", r)
			}
			if boundary == "status" {
				f.manager.onDisable = func(unit string) {
					if unit == serverUnit {
						installRetirePathWatcher(t, f, backupServiceUnit)
					}
				}
			} else {
				saved := retirement.SyncingDir
				retirement.SyncingDir = func(dir string) error {
					if dir == retirement.Root {
						status, _, err := retirement.ReadStatus()
						mustOK(t, err)
						if status.Phase == retirement.PhaseStopped {
							installRetirePathWatcher(t, f, backupServiceUnit)
						}
					}
					return nil
				}
				t.Cleanup(func() { retirement.SyncingDir = saved })
			}
			next, r := retireStop(t.Context(), j)
			if r == nil || !strings.Contains(r.Why, "operation-edge-outside-set") || next.Phase != j.Phase || requireRetireJournal(t).Phase != j.Phase {
				t.Fatalf("publication crossed active source: next=%s refusal=%+v", next.Phase, r)
			}
			status, _, err := retirement.ReadStatus()
			mustOK(t, err)
			want := retirement.PhaseIntent
			if boundary == "journal" {
				want = retirement.PhaseStopped
			}
			if status.Phase != want {
				t.Fatalf("status=%s want=%s", status.Phase, want)
			}
		})
	}
}

func TestRetirementRechecksActivationBeforeConfigMutation(t *testing.T) {
	for _, variant := range []retirement.Variant{retirement.VariantServerOnly, retirement.VariantRetainedNode} {
		t.Run(string(variant), func(t *testing.T) {
			f := newRequestFixture(t)
			if variant == retirement.VariantRetainedNode {
				f.retainANode(t)
			}
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseArchived, variant)
			mustOK(t, os.Rename(j.IdentityDir, j.Archive))
			before := mustRead(t, f.cfg)
			if variant == retirement.VariantRetainedNode {
				saved := retireBeforeConfigRename
				retireBeforeConfigRename = func() { installRetirePathWatcher(t, f, "billet-upgrade.service") }
				t.Cleanup(func() { retireBeforeConfigRename = saved })
			} else {
				installRetirePathWatcher(t, f, "billet-upgrade.service")
			}
			next, r := retireRewrite(t.Context(), retireProofMode(f), nil, j)
			if r == nil || !strings.Contains(r.Why, "operation-edge-outside-set") || next.Phase != j.Phase || mustRead(t, f.cfg) != before {
				t.Fatalf("config mutation crossed active source: next=%s refusal=%+v", next.Phase, r)
			}
			if requireRetireJournal(t).Phase != j.Phase || len(f.manager.operations) != 0 {
				t.Fatalf("config refusal advanced transition: %v", f.manager.operations)
			}
		})
	}
}

func TestRetirementRechecksNodeExecutionOnArchivedResume(t *testing.T) {
	for _, drift := range []string{"command", "arguments", "flags", "missing extended command", "KillMode"} {
		t.Run(drift, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseConfigRewritten, retirement.VariantRetainedNode)
			mustOK(t, os.Rename(j.IdentityDir, j.Archive))
			writeFile(t, f.cfg, f.rendering(t), 0o600)
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy archived resume: %+v", r)
			}
			cleanScan(t)
			mustOK(t, guardRun(t, "hold", "--holder", "ci-2", "--recover-from", requestRun, "--old-driver-stopped"))
			guard := f.guard.record(t)
			if guard.Holder != "ci-2" || !slices.Equal(guard.TakenOverFrom, []string{requestRun}) ||
				guard.Transition == nil || guard.Transition.ID != j.Provenance.TransitionID {
				t.Fatalf("production takeover did not retain the retirement: %+v", guard)
			}
			switch drift {
			case "command":
				setRetireNodeCommand(t, f, []string{"/bin/sh", "-c", "systemctl --no-block start billet-backup.service; exec " + installedBinary + " node --config " + f.cfg}, "")
			case "arguments":
				setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", f.cfg + ".other"}, "")
			case "flags":
				setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", f.cfg}, "privileged")
			case "missing extended command":
				setRetireEffect(t, f, nodeUnit, "ExecStartEx", "")
			case "KillMode":
				f.manager.set(nodeUnit, "KillMode", "process")
			}
			identity, identityRefusal := retireArchivedIdentity(j)
			if identityRefusal != nil {
				t.Fatal(identityRefusal)
			}
			mustOK(t, j.Validate(retirement.JournalExpectation{Retiring: requestRetiring, Identity: identity,
				Holder: "ci-2", TakenOverFrom: f.guard.record(t).TakenOverFrom}))
			before := mustRead(t, retirement.JournalPath())
			owner := requireRetireJournal(t).Ownership.Owner
			out, code := retiredRequest(t, f, "ci-2")
			want := retireReasonUnit
			if drift == "KillMode" {
				want = retireReasonEffects
			}
			if code != exitUnknown || retireAnswer(t, out)["reason"] != want {
				t.Fatalf("changed execution admitted on archived resume: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before ||
				requireRetireJournal(t).Ownership.Owner != owner || owner != requestRun {
				t.Fatalf("resume rebound ownership or enabled/stopped/started or advanced: %v", f.manager.operations)
			}
		})
	}
}

func TestRetirementRechecksNodeExecutionImmediatelyBeforeStart(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseConfigRewritten, retirement.VariantRetainedNode)
	f.svc.onStop = func(unit string) {
		if unit == nodeUnit {
			setRetireNodeCommand(t, f, []string{"/bin/sh", "-c", "exit 0"}, "")
		}
	}
	next, r := retireRestartNode(t.Context(), f.cfg, j)
	if r == nil || r.Reason != retireReasonUnit || next.Phase != j.Phase ||
		!slices.Contains(f.manager.operations, "stop "+nodeUnit) || slices.Contains(f.manager.operations, "start "+nodeUnit) {
		t.Fatalf("start used pre-drain execution shape: next=%s refusal=%+v operations=%v", next.Phase, r, f.manager.operations)
	}
}

func TestRetirementChecksActivationBeforePreparingManagedDirectory(t *testing.T) {
	f := newRequestFixture(t)
	installed, r := observeRetireConfig(f.cfg)
	if r != nil {
		t.Fatal(r)
	}
	if err := os.Remove(retirement.RetiredDir()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	installRetirePathWatcher(t, f, backupServiceUnit)
	f.manager.set("billet-backup.path", "ActiveState", "active")
	before, err := os.Stat(f.stateDir)
	mustOK(t, err)
	r = judgeHostPreconditions(t.Context(), retireProofMode(f), installed.cfg, &retirePlan{}, retireNow())
	if r == nil || !strings.Contains(r.Why, "operation-edge-outside-set") {
		t.Fatalf("managed directory preparation admitted active source: %+v", r)
	}
	if _, err := os.Lstat(retirement.RetiredDir()); !os.IsNotExist(err) {
		t.Fatalf("retirement directory was created before refusal: %v", err)
	}
	after, err := os.Stat(f.stateDir)
	mustOK(t, err)
	if !os.SameFile(before, after) || len(f.manager.operations) != 0 {
		t.Fatalf("preparation refusal changed identity or services: %v", f.manager.operations)
	}
}

func TestRetirementRefusesStopOnlyHelperBeforeControllerStop(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	if r := admitRetireOperation(t.Context(), j, "stop", serverUnit); r != nil {
		t.Fatalf("clean controller stop: %+v", r)
	}
	setRetireEffect(t, f, serverUnit, "PropagatesStopTo", "helper.service")
	f.manager.set("helper.service", "ActiveState", "active")
	setRetireEffect(t, f, "helper.service", "ExecStop", `"/usr/bin/true" 1 "/usr/bin/true" false 0 0 0 0 0 0 0`)
	setRetireEffect(t, f, "helper.service", "StandardOutput", "truncate")
	before := mustRead(t, retirement.JournalPath())
	r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit)
	if r == nil || !strings.Contains(r.Why, "operation-edge-outside-set: billet-server.service PropagatesStopTo=helper.service") {
		t.Fatalf("stop-only helper admitted: %+v", r)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("helper refusal stopped controller or changed phase: %v", f.manager.operations)
	}
}

func TestRetirementTimerExceptionsExpireBeforeDisable(t *testing.T) {
	for _, timer := range []string{upgradeTimerUnit, backupTimerUnit} {
		t.Run(timer, func(t *testing.T) {
			f := newRequestFixture(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
			setRetireEffect(t, f, strings.TrimSuffix(timer, ".timer")+".service", "TriggeredBy", timer)
			f.manager.set(timer, "ActiveState", "active")
			if r := admitRetireOperation(t.Context(), j, "stop", timer); r != nil {
				t.Fatalf("timer before its stop refused: %+v", r)
			}
			if r := admitRetireOperation(t.Context(), j, "disable", timer); r == nil || !strings.Contains(r.Why, "operation-reactivation") {
				t.Fatalf("already-stopped timer kept its activation exception: %+v", r)
			}
			if len(f.manager.operations) != 0 {
				t.Fatalf("admission submitted a command: %v", f.manager.operations)
			}
		})
	}
}

func TestRetirementAllowsBackupHandlingButNotStoppedProofOnResume(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseStopped, retirement.VariantServerOnly)
	f.manager.set(backupServiceUnit, "LoadState", "loaded")
	f.manager.set(backupServiceUnit, "ActiveState", "activating")
	f.manager.set(backupServiceUnit, "SubState", "start")
	f.manager.set(backupServiceUnit, "MainPID", "99")
	setRetireEffect(t, f, backupServiceUnit, "Job", "42")
	if r := admitRetireOperations(t.Context(), j, nil); r != nil {
		t.Fatalf("stopped resume intercepted backup handling: %+v", r)
	}
	if r := proveRetireStopped(t.Context(), j); r == nil || r.Reason != retireReasonStopped || !strings.Contains(r.Why, "backup completion") {
		t.Fatalf("unfinished backup reached stopped proof: %+v", r)
	}
	if len(f.manager.operations) != 0 {
		t.Fatalf("admission submitted operations: %v", f.manager.operations)
	}
}

func TestRetirementRefusesEnabledControllerCompletionAnchorBeforeStop(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	f.manager.set(serverUnit, "UnitFileState", "enabled")
	setRetireEffect(t, f, serverUnit, "WantedBy", "multi-user.target")
	if r := admitRetireOperation(t.Context(), j, "stop", serverUnit); r != nil {
		t.Fatalf("enabled controller control: %+v", r)
	}
	setRetireEffect(t, f, serverUnit, "OnSuccess", "multi-user.target")
	setRetireEffect(t, f, serverUnit, "OnSuccessJobMode", "replace")
	before := mustRead(t, retirement.JournalPath())
	r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit)
	if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "operation-edge-outside-set: billet-server.service OnSuccess=multi-user.target") {
		t.Fatalf("standard completion anchor admitted: %+v", r)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("completion refusal submitted a stop or changed phase: %v", f.manager.operations)
	}
}

func TestRetirementRefusesControllerCredentialTeardownBeforeStop(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	if r := admitRetireOperation(t.Context(), j, "stop", serverUnit); r != nil {
		t.Fatalf("clean controller: %+v", r)
	}
	// Path admission also protects resources whose current evidence is absence;
	// the real-systemd witness places an existing node file beneath this path.
	resource, err := observeRetireResource("/run/credentials/billet-server.service/node.crt")
	mustOK(t, err)
	j.RetainedInvocation.Resources = append(j.RetainedInvocation.Resources, resource)
	before := mustRead(t, retirement.JournalPath())
	r := stopAndDisableForRetirement(t.Context(), f.manager, j, serverUnit)
	if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "operation-directory-overlap") || !strings.Contains(r.Why, "CredentialDirectory=/run/credentials/billet-server.service") {
		t.Fatalf("implicit credential teardown admitted: %+v", r)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("credential refusal stopped controller or changed phase: %v", f.manager.operations)
	}
}

func TestRetirementRefusesCompletionOfBackupStartedAfterFactSnapshot(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	f.manager.set(backupServiceUnit, "LoadState", "loaded")
	f.manager.set(backupServiceUnit, "ActiveState", "inactive")
	f.manager.set(backupServiceUnit, "MainPID", "0")
	setRetireEffect(t, f, backupServiceUnit, "OnSuccess", serverUnit)
	facts, r := observeRetireFacts(t.Context(), retireProofMode(f), j)
	if r != nil {
		t.Fatal(r)
	}
	d := retirement.Decide(j.Variant, j.Phase, facts)
	if facts.Backup != retirement.BackupInactive || d.Action != retirement.ActionStop {
		t.Fatalf("inactive-backup snapshot did not select shutdown: %+v", d)
	}
	// Timer fires after the driver's snapshot. It remains in flight until
	// controller shutdown; the fake returns observations and judges no policy.
	f.manager.set(backupServiceUnit, "ActiveState", "activating")
	f.manager.set(backupServiceUnit, "SubState", "start")
	f.manager.set(backupServiceUnit, "MainPID", "99")
	setRetireEffect(t, f, backupServiceUnit, "Job", "42")
	completed := false
	f.manager.onDisable = func(unit string) {
		if unit != serverUnit {
			return
		}
		completed = true
		f.manager.set(backupServiceUnit, "ActiveState", "inactive")
		f.manager.set(backupServiceUnit, "SubState", "dead")
		f.manager.set(backupServiceUnit, "MainPID", "0")
		setRetireEffect(t, f, backupServiceUnit, "Job", "")
		f.manager.set(serverUnit, "ActiveState", "active")
		f.manager.set(serverUnit, "MainPID", "100")
	}
	before := mustRead(t, retirement.JournalPath())
	next, r := retireStop(t.Context(), j)
	if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "operation-edge-outside-set: billet-backup.service OnSuccess=billet-server.service") {
		t.Fatalf("in-flight backup completion was not admitted before shutdown: %+v", r)
	}
	if completed || len(f.manager.operations) != 0 || next.Phase != j.Phase || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("backup completion refusal came after controller shutdown: completed=%v phase=%s operations=%v", completed, next.Phase, f.manager.operations)
	}
}
