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

func TestRetirementEnvironmentSpecsPreserveEveryOptionalityFlag(t *testing.T) {
	for _, test := range []struct {
		name  string
		lines []string
		want  []environmentFileSpec
	}{
		{name: "empty", lines: []string{""}},
		{name: "required", lines: []string{"/etc/billet/node.env (ignore_errors=no)"}, want: []environmentFileSpec{{Path: "/etc/billet/node.env"}}},
		{name: "optional", lines: []string{"/run/billet/node.env (ignore_errors=yes)"}, want: []environmentFileSpec{{Path: "/run/billet/node.env", IgnoreErrors: true}}},
		{name: "all entries", lines: []string{"/etc/one (ignore_errors=no)", "/run/two (ignore_errors=yes)", "/etc/three (ignore_errors=no)"}, want: []environmentFileSpec{{Path: "/etc/one"}, {Path: "/run/two", IgnoreErrors: true}, {Path: "/etc/three"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := environmentFileSpecsOfAll(test.lines)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("typed environment files = %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
	for _, lines := range [][]string{nil, {" "}, {"", "/etc/node.env (ignore_errors=no)"}, {"/etc/node.env"}, {"/etc/node.env (ignore_errors=maybe)"}, {"relative (ignore_errors=no)"}, {"/etc/*.env (ignore_errors=no)"}} {
		if _, err := environmentFileSpecsOfAll(lines); err == nil || !strings.Contains(err.Error(), "retained-input-environment-unknown") {
			t.Fatalf("unknown environment evidence admitted: %v: %v", lines, err)
		}
	}
}

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
				effects := filepath.Join(f.unitsDir, nodeUnit+".effects")
				body := mustRead(t, effects)
				body = strings.ReplaceAll(body, "EnvironmentFiles="+path+" (ignore_errors=no)\n", "")
				if scenario == "property unreadable" {
					body += "EnvironmentFiles=unreadable-property\n"
				}
				writeFile(t, effects, body, 0o644)
			}
			queryFailed := false
			if scenario == "property query failure" {
				saved := retireOperationInspector
				retireOperationInspector = func() *lifeops.Inspector {
					i := saved()
					lifeops.WithObserver(func(_ context.Context, args []string) {
						if !queryFailed && slices.Contains(args, "--property=EnvironmentFiles") {
							queryFailed = true
							mustOK(t, os.Remove(filepath.Join(f.unitsDir, nodeUnit+".effects")))
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
			if scenario == "mandatory runtime" {
				reason = "retained-input-volatile"
			} else if scenario == "mandatory absent" {
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
