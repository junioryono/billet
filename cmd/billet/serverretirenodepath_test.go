package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

func setRetireNodePathDropIn(t *testing.T, f *requestFixture, directive, path string) {
	t.Helper()
	section := "Service"
	if directive == "ConditionPathExists" {
		section = "Unit"
		body, err := json.Marshal(map[string]any{"type": "a(sbbsi)", "data": []any{[]any{directive, false, false, path, 0}}})
		mustOK(t, err)
		writeFile(t, filepath.Join(f.unitsDir, nodeUnit+".Conditions.json"), string(body), 0o600)
		setRetireEffect(t, f, nodeUnit, "Conditions", "[unprintable]")
	} else {
		setRetireEffect(t, f, nodeUnit, directive, path)
	}
	dropIn := filepath.Join(f.unitsDir, "node-path.conf")
	writeFile(t, dropIn, "["+section+"]\n"+directive+"="+path+"\n", 0o644)
	setRetireEffect(t, f, nodeUnit, "DropInPaths", dropIn)
}

func TestRetirementRefusesNodeUnitPathDependenciesBeforeIntent(t *testing.T) {
	for _, scenario := range []string{"ReadWritePaths", "ConditionPathExists", "volatile", "optional archived"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			path, directive, reason := f.stateDir, scenario, "retained-node-path-archived"
			if scenario == "volatile" {
				path, directive, reason = "/run/billet/somewhere-else", "ReadWritePaths", "retained-input-volatile"
			} else if scenario == "optional archived" {
				path, directive = "-"+path, "ReadWritePaths"
			}
			setRetireNodePathDropIn(t, f, directive, path)
			before := mustRead(t, f.cfg)
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			if code != exitUnknown || !strings.Contains(out, reason) {
				t.Fatalf("node unit path admitted: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, f.cfg) != before {
				t.Fatalf("path refusal followed mutation: %v", f.manager.operations)
			}
			if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
				t.Fatalf("path refusal published intent: %v %v", presence, err)
			}
			if _, err := os.Stat(f.stateDir); err != nil {
				t.Fatalf("path refusal followed archive: %v", err)
			}
		})
	}
}

func TestRetirementRechecksNodeUnitPathsAtStoppedBoundaries(t *testing.T) {
	for _, boundary := range []string{"status", "journal", "archive"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			mustOK(t, retirement.WriteStatus(j.Phase, j.Variant, retireNow()))
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("clean admission: %+v", r)
			}
			introduced := false
			move := func() {
				if !introduced {
					introduced = true
					setRetireNodePathDropIn(t, f, "ReadWritePaths", f.stateDir)
				}
			}
			savedProof, savedSync := retireBeforeStoppedProof, retirement.SyncingDir
			retireBeforeStoppedProof = func() {
				if boundary == "status" || (boundary == "archive" && requireRetireJournal(t).Phase == retirement.PhaseStopped) {
					move()
				}
			}
			retirement.SyncingDir = func(dir string) error {
				if boundary == "journal" && dir == retirement.Root {
					status, _, err := retirement.ReadStatus()
					mustOK(t, err)
					if status.Phase == retirement.PhaseStopped {
						move()
					}
				}
				return nil
			}
			t.Cleanup(func() { retireBeforeStoppedProof, retirement.SyncingDir = savedProof, savedSync })
			next, r := retireStop(t.Context(), j)
			if boundary == "archive" {
				if r != nil {
					t.Fatalf("clean stopped proof: %+v", r)
				}
				j = next
				next, r = retireArchive(t.Context(), j)
			}
			if !introduced || r == nil || r.Reason != retireReasonStopped || !strings.Contains(r.Why, "retained-node-path-archived") || next.Phase != j.Phase || requireRetireJournal(t).Phase != j.Phase {
				t.Fatalf("node property drift crossed boundary: %+v %s", r, next.Phase)
			}
			if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
				t.Fatalf("node property drift crossed archive: %v", err)
			}
			status, _, err := retirement.ReadStatus()
			mustOK(t, err)
			want := retirement.PhaseStopped
			if boundary == "status" {
				want = retirement.PhaseIntent
			}
			if status.Phase != want {
				t.Fatalf("status advanced: %s", status.Phase)
			}
		})
	}
}

func TestRetirementResumeBindsBothConfigOperandsToTheJournal(t *testing.T) {
	for _, phase := range []retirement.Phase{retirement.PhaseIntent, retirement.PhaseStopped} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, phase, retirement.VariantRetainedNode)
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("clean resume: %+v", r)
			}
			body := mustRead(t, f.cfg)
			f.cfg = filepath.Join(f.stateDir, "resume.yaml")
			writeFile(t, f.cfg, body, 0o600)
			setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", f.cfg}, "")
			before := mustRead(t, retirement.JournalPath())
			out, code := retainedResume(t, f)
			if code != exitUnknown || retireAnswer(t, out)["reason"] != "retained-config-path-changed" {
				t.Fatalf("both moved operands bypassed recorded config: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before || mustRead(t, f.cfg) != body {
				t.Fatalf("moved config resume mutated state: %v", f.manager.operations)
			}
			props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "InvocationID")
			mustOK(t, err)
			if firstProp(props, "InvocationID") != j.RetainedInvocation.InvocationID {
				t.Fatal("witness did not preserve the original invocation")
			}
			if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
				t.Fatalf("moved config resume archived identity: %v", err)
			}
		})
	}
}

func TestRetirementRefusesLeafSymlinkConfigurationBeforeIntent(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	target := f.cfg
	f.cfg = filepath.Join(t.TempDir(), "billet.yaml")
	mustOK(t, os.Symlink(target, f.cfg))
	setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", f.cfg}, "")
	body := mustRead(t, target)
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	if code != exitUnknown || retireAnswer(t, out)["reason"] != "retained-config-symlink" || !strings.Contains(out, target) {
		t.Fatalf("leaf symlink admitted or direct target not named: %s", out)
	}
	if len(f.manager.operations) != 0 || mustRead(t, target) != body {
		t.Fatalf("leaf symlink refusal mutated state: %v", f.manager.operations)
	}
	if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
		t.Fatalf("leaf symlink refusal published intent: %v %v", presence, err)
	}
}

func TestRetirementResumeBindsLexicalAndResolvedConfigNames(t *testing.T) {
	for _, spelling := range []string{"lexical alias", "retargeted parent"} {
		t.Run(spelling, func(t *testing.T) {
			f := newRequestFixture(t)
			parent := filepath.Join(t.TempDir(), "billet")
			mustOK(t, os.Symlink(filepath.Dir(f.cfg), parent))
			if spelling == "retargeted parent" {
				f.cfg = filepath.Join(parent, filepath.Base(f.cfg))
			}
			retainAndRestartANode(t, f)
			f.reserve(t)
			j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("clean pathname control: %+v", r)
			}
			if spelling == "lexical alias" {
				f.cfg = filepath.Join(parent, filepath.Base(f.cfg))
				setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", f.cfg}, "")
			} else {
				other := t.TempDir()
				mustOK(t, os.Link(f.cfg, filepath.Join(other, filepath.Base(f.cfg))))
				mustOK(t, os.Remove(parent))
				mustOK(t, os.Symlink(other, parent))
			}
			before := mustRead(t, retirement.JournalPath())
			out, code := retainedResume(t, f)
			if code != exitUnknown || retireAnswer(t, out)["reason"] != "retained-config-path-changed" {
				t.Fatalf("same inode bypassed pathname binding: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
				t.Fatalf("pathname refusal followed a mutation: %v", f.manager.operations)
			}
		})
	}
}

func TestRetirementRechecksConfigBindingAfterWaits(t *testing.T) {
	for _, boundary := range []string{"stop wait", "rewrite flush"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if boundary == "rewrite flush" {
				var r *retireRefusal
				j, r = retireStop(t.Context(), j)
				if r != nil {
					t.Fatal(r)
				}
				j, r = retireArchive(t.Context(), j)
				if r != nil {
					t.Fatal(r)
				}
			}
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("clean binding control: %+v", r)
			}
			moved := false
			move := func() {
				moved = true
				other := f.cfg + ".other"
				writeFile(t, other, mustRead(t, f.cfg), 0o600)
				setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", other}, "")
			}
			before := mustRead(t, retirement.JournalPath())
			operations := len(f.manager.operations)
			var next retirement.Journal
			var r *retireRefusal
			if boundary == "stop wait" {
				f.svc.onStop = func(unit string) {
					if unit == upgradeTimerUnit {
						move()
					}
				}
				next, r = retireStop(t.Context(), j)
				operations++
			} else {
				saved := retireBeforeConfigRename
				retireBeforeConfigRename = move
				t.Cleanup(func() { retireBeforeConfigRename = saved })
				next, r = retireRewrite(t.Context(), retireProofMode(f), nil, j)
			}
			if !moved || r == nil || r.Reason != "retained-config-path-changed" || next.Phase != j.Phase || len(f.manager.operations) != operations || mustRead(t, retirement.JournalPath()) != before {
				t.Fatalf("binding drift crossed mutation: %+v %s %v", r, next.Phase, f.manager.operations)
			}
		})
	}
}

func TestRetirementParentSymlinkConfigurationSurvivesRewriteAndResume(t *testing.T) {
	for _, crash := range []bool{false, true} {
		t.Run(map[bool]string{false: "rewrite", true: "crash after rename"}[crash], func(t *testing.T) {
			f := newRequestFixture(t)
			parent := filepath.Join(t.TempDir(), "billet")
			mustOK(t, os.Symlink(filepath.Dir(f.cfg), parent))
			f.cfg = filepath.Join(parent, filepath.Base(f.cfg))
			retainAndRestartANode(t, f)
			f.reserve(t)
			var out string
			var code int
			if crash {
				j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
				j, r := retireStop(t.Context(), j)
				if r != nil {
					t.Fatal(r)
				}
				j, r = retireArchive(t.Context(), j)
				if r != nil {
					t.Fatal(r)
				}
				saved := retireSyncDir
				retireSyncDir = func(string) error { return errors.New("fixture interrupted after config rename") }
				t.Cleanup(func() { retireSyncDir = saved })
				next, r := retireRewrite(t.Context(), retireProofMode(f), nil, j)
				if r == nil || !strings.Contains(r.Why, "fixture interrupted") || next.Phase != retirement.PhaseArchived {
					t.Fatalf("missed config rename crash window: %+v %s", r, next.Phase)
				}
				retireSyncDir = saved
				out, code = retainedResume(t, f)
			} else {
				out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			}
			retiredAnswer(t, out, code)
			if info, err := os.Lstat(parent); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("rewrite replaced parent symlink: %v", err)
			}
			now, err := observeRetireResource(f.cfg)
			mustOK(t, err)
			j := requireRetireJournal(t)
			if j.RetainedInvocation.ConfigReplacement == nil || *j.RetainedInvocation.ConfigReplacement != now {
				t.Fatal("parent symlink rewrite lost its recorded replacement identity")
			}
		})
	}
}
