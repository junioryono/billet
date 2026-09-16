package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

// The full command supplies the production RequiredInputs/UnitPaths split and
// the shipped node runtime directories. Dropping environment capture admits the
// mandatory volatile witness; treating optional files as required kills controls.
func TestRetirementProtectsMandatoryEnvironmentFilesThroughTheHandoff(t *testing.T) {
	for _, scenario := range []string{"mandatory runtime", "mandatory persistent", "optional present", "optional absent", "optional own runtime", "mandatory absent", "property missing", "property unreadable", "property query failure"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			setRetireEffect(t, f, nodeUnit, "RuntimeDirectory", "billet/locks billet/registration")
			path := filepath.Join(f.unitsDir, "volatile", "run", "billet", "locks", "node.env")
			if scenario == "mandatory persistent" || scenario == "mandatory absent" || strings.HasPrefix(scenario, "optional") {
				path = filepath.Join(t.TempDir(), "etc", "billet", "node.env")
			}
			if scenario == "optional own runtime" {
				path = "/run/billet/locks/node.env"
			} else {
				mustOK(t, os.MkdirAll(filepath.Dir(path), 0o700))
			}
			if scenario != "optional absent" && scenario != "mandatory absent" && scenario != "optional own runtime" {
				writeFile(t, path, "", 0o600)
			}
			optional := strings.HasPrefix(scenario, "optional")
			flag := "no"
			if optional {
				flag = "yes"
			}
			setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", path+" (ignore_errors="+flag+")")
			if scenario == "property missing" || scenario == "property unreadable" {
				typed := filepath.Join(f.unitsDir, nodeUnit+".EnvironmentFiles.json")
				mustOK(t, os.Remove(typed))
				if scenario == "property unreadable" {
					writeFile(t, typed, `{"type":"a(sb)","data":[["/etc/node.env","false"]]}`, 0o600)
				}
			}
			queryFailed := false
			if scenario == "property query failure" {
				saved := retireOperationInspector
				retireOperationInspector = func() *lifeops.Inspector {
					i := saved()
					lifeops.WithObserver(func(_ context.Context, args []string) {
						if !queryFailed && slices.Contains(args, "EnvironmentFiles") {
							queryFailed = true
							mustOK(t, os.Remove(filepath.Join(f.unitsDir, nodeUnit+".EnvironmentFiles.json")))
						}
					})(i)
					return i
				}
				t.Cleanup(func() { retireOperationInspector = saved })
			}
			before := mustRead(t, f.cfg)
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			if scenario == "mandatory persistent" || optional {
				retiredAnswer(t, out, code)
				if !slices.Contains(f.manager.operations, "stop "+nodeUnit) || !slices.Contains(f.manager.operations, "start "+nodeUnit) {
					t.Fatalf("supported environment control skipped handoff: %v", f.manager.operations)
				}
				j := requireRetireJournal(t)
				captured := slices.ContainsFunc(j.RetainedInvocation.Resources, func(r retirement.RetainedResource) bool { return r.Path == path && !r.Runtime && !r.Absent })
				if captured == optional {
					t.Fatalf("wrong environment requirement classification: optional=%v captured=%v", optional, captured)
				}
				return
			}
			reason := "retained-input-environment-unknown"
			switch scenario {
			case "mandatory runtime":
				reason = "retained-input-volatile"
			case "mandatory absent":
				reason = "retained-input-unreadable"
			}
			if scenario == "property query failure" && !queryFailed {
				t.Fatal("fixture did not fail the requested EnvironmentFiles read")
			}
			if code != exitUnknown || !strings.Contains(out, reason) {
				t.Fatalf("mandatory environment refusal missing: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, f.cfg) != before {
				t.Fatalf("environment refusal followed a mutation: %v", f.manager.operations)
			}
			if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
				t.Fatalf("environment refusal published intent: %v %v", presence, err)
			}
		})
	}
}

func TestRetirementRechecksEnvironmentFilesAfterAdmissionAndStopWaits(t *testing.T) {
	for _, mutation := range []string{"new required file", "replaced required file", "optional becomes required"} {
		t.Run(mutation, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			path := filepath.Join(t.TempDir(), "node.env")
			writeFile(t, path, "", 0o600)
			flag := "no"
			if mutation == "optional becomes required" {
				flag = "yes"
			}
			setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", path+" (ignore_errors="+flag+")")
			installed, r := observeRetireConfig(f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			f.originalNode, r = captureRetireInvocation(t.Context(), installed.cfg, f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy environment admission: %+v", r)
			}
			f.svc.onStop = func(unit string) {
				if unit != upgradeTimerUnit {
					return
				}
				if mutation == "replaced required file" {
					writeFile(t, path+".replacement", "", 0o600)
					mustOK(t, os.Rename(path+".replacement", path))
				} else {
					if mutation == "new required file" {
						path += ".new"
						writeFile(t, path, "", 0o600)
					}
					setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", path+" (ignore_errors=no)")
				}
			}
			next, r := retireStop(t.Context(), j)
			if r == nil || r.Reason != retireReasonEffects || next.Phase != j.Phase || !reflect.DeepEqual(f.manager.operations, []string{"stop " + upgradeTimerUnit}) {
				t.Fatalf("environment drift crossed the next operation: %+v %s %v", r, next.Phase, f.manager.operations)
			}
		})
	}
}

// Reverting the command to text EnvironmentFiles refuses this stock control;
// accepting every read instead is killed by the malformed/query-failure cases.
func TestRetirementWithoutEnvironmentFilesCompletesThroughProductionReaders(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "EnvironmentFiles")
	mustOK(t, err)
	if _, exists := props["EnvironmentFiles"]; exists {
		t.Fatalf("fake invented an empty text property: %v", props)
	}
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	retiredAnswer(t, out, code)
	j := requireRetireJournal(t)
	if j.Phase != retirement.PhaseDone || !slices.Contains(f.manager.operations, "stop "+nodeUnit) || !slices.Contains(f.manager.operations, "start "+nodeUnit) {
		t.Fatalf("clean retirement did not complete the node handoff: %s %v", j.Phase, f.manager.operations)
	}
	calls := mustRead(t, filepath.Join(f.unitsDir, ".manager-calls"))
	if !strings.Contains(calls, "org.freedesktop.systemd1.Service EnvironmentFiles\n") {
		t.Fatal("retirement never used the production typed environment reader")
	}
}

func TestRetainedEnvironmentReaderPreservesAbsenceAndReadFailures(t *testing.T) {
	for _, c := range []struct {
		name      string
		optional  bool
		present   bool
		link      bool
		wantError bool
	}{
		{"required present", false, true, false, false},
		{"optional present", true, true, false, false},
		{"optional absent", true, false, false, false},
		{"required absent", false, false, false, true},
		{"optional dangling link", true, false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.env")
			if c.present {
				writeFile(t, path, "NODE_MODE=worker\n", 0o600)
			}
			if c.link {
				mustOK(t, os.Symlink("missing-target", path))
			}
			body, err := readRetireEnvironment(lifeops.EnvironmentFile{Path: path, IgnoreErrors: c.optional})
			if (err != nil) != c.wantError {
				t.Fatalf("read error=%v, want error=%v", err, c.wantError)
			}
			if c.present && string(body) != "NODE_MODE=worker\n" {
				t.Fatal("reader did not read the existing file")
			}
			if !c.present && !c.link {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("reader created an absent environment file: %v", err)
				}
			}
		})
	}
}

func TestRetainedEnvironmentReaderBoundsExistingOptionalFiles(t *testing.T) {
	for _, shape := range []string{"directory", "oversized"} {
		t.Run(shape, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.env")
			if shape == "directory" {
				mustOK(t, os.Mkdir(path, 0o700))
			} else {
				writeFile(t, path, strings.Repeat("x", maxEnvironmentBytes+1), 0o600)
			}
			if _, err := readRetireEnvironment(lifeops.EnvironmentFile{Path: path, IgnoreErrors: true}); err == nil {
				t.Fatal("optional file hid a failed or over-bound read")
			}
		})
	}
}
