package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
)

func TestRetirementRefusesControllerAliasOfNodeOnIntentResume(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := plantResumedRetirement(t, f, retirement.PhaseIntent, retirement.VariantRetainedNode)
	if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
		t.Fatalf("healthy intent control: %+v", r)
	}
	// A loaded alias must refuse even when the recorded installation sources
	// still match; replacing FragmentPath would test source drift first.
	setRetireEffect(t, f, nodeUnit, "Names", nodeUnit+" "+serverUnit)
	if r := admitRetireInertReload(t.Context(), retireOperationInspector(), j, retireInertRecord{Sources: j.InertSources}); r != nil {
		t.Fatalf("alias fixture changed reload evidence: %+v", r)
	}
	before := mustRead(t, retirement.JournalPath())
	out, code := retiredRequest(t, f, requestRun)
	if code != exitUnknown || retireAnswer(t, out)["reason"] != retireReasonEffects || !strings.Contains(out, "operation-role-collision") {
		t.Fatalf("cross-role alias was admitted on resume: %s", out)
	}
	if len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
		t.Fatalf("alias refusal stopped controller alias or changed intent: %v", f.manager.operations)
	}
	if r := proveRetireInvocation(t.Context(), j.RetainedInvocation); r != nil {
		t.Fatalf("alias refusal changed the retained invocation: %+v", r)
	}
}

func TestRetirementRefusesReloadPreservedPrivateTmpBeforeControllerStop(t *testing.T) {
	for _, owner := range []string{serverUnit, backupServiceUnit} {
		t.Run(owner, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy teardown control: %+v", r)
			}
			root := filepath.Join(f.unitsDir, "volatile")
			bootID := "01234567-89ab-cdef-0123-456789abcdef"
			bootPath := filepath.Join(root, "boot_id")
			mustOK(t, os.MkdirAll(root, 0o700))
			writeFile(t, bootPath, bootID+"\n", 0o644)
			tmp, varTmp := filepath.Join(root, "tmp"), filepath.Join(root, "var", "tmp")
			mustOK(t, os.MkdirAll(tmp, 0o700))
			tree := filepath.Join(varTmp, "systemd-private-"+strings.ReplaceAll(bootID, "-", "")+"-"+owner+"-fixture", "tmp")
			mustOK(t, os.MkdirAll(tree, 0o700))
			saved := retireOperationInspector
			retireOperationInspector = func() *lifeops.Inspector {
				i := saved()
				lifeops.WithOperationTemporaryDirectories(bootPath, tmp, varTmp)(i)
				return i
			}
			t.Cleanup(func() { retireOperationInspector = saved })
			installed, r := observeRetireConfig(f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			configBody := mustRead(t, f.cfg)
			for _, path := range []string{installed.cfg.Node.TLS.CertPath, installed.cfg.Node.TLS.KeyPath} {
				destination := filepath.Join(tree, filepath.Base(path))
				writeFile(t, destination, mustRead(t, path), 0o600)
				configBody = strings.ReplaceAll(configBody, path, destination)
				resource, err := observeRetireResource(destination)
				mustOK(t, err)
				j.RetainedInvocation.Resources = append(j.RetainedInvocation.Resources, resource)
			}
			writeFile(t, f.cfg, configBody, 0o600)
			// The volatile inputs belong to this intent's installed configuration;
			// an unrelated digest mismatch must not mask their teardown refusal.
			j.InstalledSHA256 = f.installedSHA(t)
			mustOK(t, j.Write(retireNow()))
			// Loaded settings after yes -> daemon-reload -> no retain the old tree.
			setRetireEffect(t, f, owner, "PrivateTmp", "no")
			if owner == backupServiceUnit {
				f.manager.set(owner, "LoadState", "loaded")
				f.manager.set(owner, "UnitFileState", "static")
				f.manager.set(owner, "ActiveState", "activating")
				f.manager.set(owner, "SubState", "start")
				f.manager.set(owner, "MainPID", "99")
				setRetireEffect(t, f, owner, "Job", "42")
			}
			before := mustRead(t, retirement.JournalPath())
			next, r := retireStop(t.Context(), j)
			if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "retained-input-volatile") {
				t.Fatalf("private temporary TLS teardown admitted: %+v", r)
			}
			if next.Phase != j.Phase || len(f.manager.operations) != 0 || mustRead(t, retirement.JournalPath()) != before {
				t.Fatalf("teardown refusal followed a stop or phase change: %v", f.manager.operations)
			}
			for _, resource := range j.RetainedInvocation.Resources {
				if resource.Absent {
					continue
				}
				if _, err := os.Stat(resource.Path); err != nil {
					t.Fatalf("private temporary refusal lost %s: %v", resource.Path, err)
				}
			}
			props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "InvocationID")
			mustOK(t, err)
			if firstProp(props, "InvocationID") != retainedInvocation {
				t.Fatal("private temporary refusal changed the node invocation")
			}
		})
	}
}

func installRetirePreparationHelper(t *testing.T, f *requestFixture) {
	t.Helper()
	installRetirePathWatcher(t, f, "helper.service")
	f.manager.set("billet-backup.path", "ActiveState", "active")
	writeFile(t, filepath.Join(f.unitsDir, "watcher-source"), "[Path]\nPathExists="+retirement.RetiredDir()+"\nUnit=helper.service\n", 0o644)
	setRetireEffect(t, f, "helper.service", "OnSuccess", backupServiceUnit)
	f.manager.set(backupServiceUnit, "LoadState", "loaded")
	f.manager.set(backupServiceUnit, "UnitFileState", "static")
	setRetireEffect(t, f, backupServiceUnit, "OnSuccessOf", "helper.service")
}

func TestRetirementChecksClosedEdgesBeforePreparingManagedDirectory(t *testing.T) {
	f := newRequestFixture(t)
	installed, r := observeRetireConfig(f.cfg)
	if r != nil {
		t.Fatal(r)
	}
	if err := os.Remove(retirement.RetiredDir()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	installRetirePreparationHelper(t, f)
	r = judgeHostPreconditions(t.Context(), retireProofMode(f), installed.cfg, &retirePlan{}, retireNow())
	if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "operation-edge-outside-set: billet-backup.service OnSuccessOf=helper.service") {
		t.Fatalf("helper-mediated preparation admitted: %+v", r)
	}
	if _, err := os.Lstat(retirement.RetiredDir()); !os.IsNotExist(err) {
		t.Fatalf("retirement directory created before closed-edge refusal: %v", err)
	}
	if len(f.manager.operations) != 0 {
		t.Fatalf("preparation submitted operations: %v", f.manager.operations)
	}
}

func TestRetirementChecksClosedEdgesBeforeRequestLockPreparation(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)
	input := f.input(t, nil)
	installRetirePreparationHelper(t, f)
	var mutations []guardOp
	saved := guardHook
	guardHook = func(op guardOp) error {
		if op.Kind == "mkdir" || op.Kind == "openat" || op.Kind == "fsync" {
			mutations = append(mutations, op)
		}
		return nil
	}
	t.Cleanup(func() { guardHook = saved })
	out, code := f.request(t, input)
	if code != exitUnknown || retireAnswer(t, out)["reason"] != retireReasonEffects || !strings.Contains(out, "OnSuccessOf=helper.service") {
		t.Fatalf("request preparation admitted helper: %s", out)
	}
	if len(mutations) != 0 || len(f.manager.operations) != 0 {
		t.Fatalf("closed-edge admission followed lock preparation: %v %v", mutations, f.manager.operations)
	}
}

func TestRetirementRechecksClosedEdgesBeforeStaging(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	input := f.input(t, f.retainedOverrides(t))
	introduced := false
	saved := guardHook
	guardHook = func(op guardOp) error {
		if op.Kind == "rename" && op.Path == filepath.Join(f.guard.active(), guardRecordName) {
			introduced = true
			installRetirePreparationHelper(t, f)
		}
		return nil
	}
	t.Cleanup(func() { guardHook = saved })
	out, code := f.retainedRequest(t, input)
	if !introduced || code != exitUnknown || retireAnswer(t, out)["reason"] != retireReasonEffects || !strings.Contains(out, "OnSuccessOf=helper.service") {
		t.Fatalf("staging failed to recheck closed edges after marker persistence: %s", out)
	}
	if _, err := os.Lstat(retirement.StagePath()); !os.IsNotExist(err) {
		t.Fatalf("staging mutation preceded closed-edge refusal: %v", err)
	}
	if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent || len(f.manager.operations) != 0 {
		t.Fatalf("staging refusal published intent or submitted operations: %v %v %v", presence, err, f.manager.operations)
	}
}

func TestRetirementRechecksEffectsBeforeRecordingTimerStop(t *testing.T) {
	f := newRequestFixture(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantServerOnly)
	j.TimerStoppedAt = ""
	mustOK(t, j.Write(retireNow()))
	before := mustRead(t, retirement.JournalPath())
	introduced := false
	f.manager.onDisable = func(unit string) {
		if unit == backupTimerUnit {
			introduced = true
			installRetirePreparationHelper(t, f)
		}
	}
	next, r := retireStop(t.Context(), j)
	if !introduced || r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "OnSuccessOf=helper.service") {
		t.Fatalf("timer metadata crossed changed closed edges: %+v", r)
	}
	if next.Phase != j.Phase || mustRead(t, retirement.JournalPath()) != before {
		t.Fatal("timer stop was journaled before fresh effect admission")
	}
	for _, operation := range f.manager.operations {
		if operation == "stop "+serverUnit {
			t.Fatal("controller stopped after timer metadata refusal")
		}
	}
}

func TestRetirementRechecksEffectsAfterConfigurationFlush(t *testing.T) {
	for _, variant := range []retirement.Variant{retirement.VariantServerOnly, retirement.VariantRetainedNode} {
		t.Run(string(variant), func(t *testing.T) {
			f, j := retireProofHost(t, variant, retirement.PhaseArchived)
			before := mustRead(t, retirement.JournalPath())
			introduced := false
			saved := retireSyncDir
			retireSyncDir = func(dir string) error {
				if dir == filepath.Dir(f.cfg) {
					introduced = true
					installRetirePreparationHelper(t, f)
				}
				return nil
			}
			t.Cleanup(func() { retireSyncDir = saved })
			next, r := performRetireAction(t.Context(), retireProofMode(f), nil, j, retirement.ActionAdvanceRewritten)
			if !introduced || r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "OnSuccessOf=helper.service") {
				t.Fatalf("rewrite phase crossed changed closed edges: %+v", r)
			}
			if next.Phase != j.Phase || mustRead(t, retirement.JournalPath()) != before {
				t.Fatal("configuration flush authorized a later phase write")
			}
		})
	}
}

func TestRetirementRechecksEffectsAfterReplacementJournal(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseArchived, retirement.VariantRetainedNode)
	mustOK(t, os.Rename(j.IdentityDir, j.Archive))
	before := mustRead(t, f.cfg)
	introduced := false
	saved := retirement.SyncingDir
	retirement.SyncingDir = func(dir string) error {
		if dir == retirement.RetiredDir() && requireRetireJournal(t).RetainedInvocation.ConfigReplacement != nil {
			introduced = true
			installRetirePreparationHelper(t, f)
		}
		return nil
	}
	t.Cleanup(func() { retirement.SyncingDir = saved })
	next, r := retireRewrite(t.Context(), retireProofMode(f), nil, j)
	if !introduced || r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "OnSuccessOf=helper.service") {
		t.Fatalf("configuration rename crossed replacement-journal drift: %+v", r)
	}
	if next.Phase != j.Phase || requireRetireJournal(t).Phase != j.Phase || mustRead(t, f.cfg) != before {
		t.Fatal("replacement journal persistence authorized configuration rename")
	}
}

func TestDoneRetirementRechecksEffectsBeforeSettlementWrites(t *testing.T) {
	for _, boundary := range []string{"marker", "settled journal"} {
		t.Run(boundary, func(t *testing.T) {
			f, j := retireProofHost(t, retirement.VariantRetainedNode, retirement.PhaseDone)
			j.RowDone, j.CompletedBy = true, requestRetiring
			mustOK(t, j.Write(retireNow()))
			m := retireProofMode(f)
			root, dir, _, r := retireGuard(m.run)
			if r != nil {
				t.Fatal(r)
			}
			defer root.release()
			defer func() { _ = dir.Close() }()
			if boundary == "settled journal" {
				if _, r := retireClearMarker(t.Context(), m, root, dir, j); r != nil {
					t.Fatalf("healthy marker control: %+v", r)
				}
			}
			beforeJournal := mustRead(t, retirement.JournalPath())
			beforeGuard := mustRead(t, filepath.Join(f.guard.active(), guardRecordName))
			installRetirePreparationHelper(t, f)
			if boundary == "marker" {
				_, r = retireClearMarker(t.Context(), m, root, dir, j)
			} else {
				r = retireMarkSettled(t.Context(), m, &j)
			}
			if r == nil || r.Reason != retireReasonEffects || !strings.Contains(r.Why, "OnSuccessOf=helper.service") {
				t.Fatalf("settlement crossed changed closed edges: %+v", r)
			}
			if mustRead(t, retirement.JournalPath()) != beforeJournal ||
				mustRead(t, filepath.Join(f.guard.active(), guardRecordName)) != beforeGuard {
				t.Fatal("settlement published before full effect admission")
			}
		})
	}
}
