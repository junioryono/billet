package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

func TestRetirementKeepsRequiredTLSOutOfDisposableRuntimeRecords(t *testing.T) {
	for _, hazard := range []string{"own runtime", "controller runtime traversal"} {
		t.Run(hazard, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			installed, r := observeRetireConfig(f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			original := mustRead(t, f.cfg)
			info, err := os.Stat(f.cfg)
			mustOK(t, err)
			// The fixture maps /run into its private host tree. The loaded node
			// uses the shipped RuntimeDirectory list and production classification.
			run := filepath.Join(f.unitsDir, "volatile", "run", "billet")
			tls := filepath.Join(run, "locks", "tls")
			setRetireEffect(t, f, nodeUnit, "RuntimeDirectory", "billet/locks billet/registration")
			if hazard == "controller runtime traversal" {
				tls = filepath.Join(run, "controller-runtime", "tls")
				mustOK(t, os.MkdirAll(filepath.Dir(tls), 0o700))
				mustOK(t, os.Symlink(filepath.Dir(installed.cfg.Node.TLS.KeyPath), tls))
				setRetireEffect(t, f, serverUnit, "RuntimeDirectory", "billet/controller-runtime")
			} else {
				mustOK(t, os.MkdirAll(tls, 0o700))
			}
			body := original
			for _, path := range []string{installed.cfg.Node.TLS.CertPath, installed.cfg.Node.TLS.KeyPath, installed.cfg.Node.TLS.CAPath} {
				destination := filepath.Join(tls, filepath.Base(path))
				if hazard == "own runtime" {
					writeFile(t, destination, mustRead(t, path), 0o600)
				}
				body = strings.ReplaceAll(body, path, destination)
			}
			writeFile(t, f.cfg, body, 0o600)
			mustOK(t, os.Chtimes(f.cfg, info.ModTime(), info.ModTime()))
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			if code != exitUnknown || retireAnswer(t, out)["reason"] != retireReasonEffects || !strings.Contains(out, "retained-input-volatile") {
				t.Fatalf("full request admitted volatile TLS: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, f.cfg) != body {
				t.Fatalf("volatile input refusal followed an operation or config rewrite: %v", f.manager.operations)
			}
			if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
				t.Fatalf("volatile input refusal published intent: %v %v", presence, err)
			}
			props, err := retireOperationInspector().UnitProperties(t.Context(), nodeUnit, "InvocationID")
			mustOK(t, err)
			if firstProp(props, "InvocationID") != retainedInvocation {
				t.Fatal("volatile input refusal changed the original node invocation")
			}
			// Persistent credentials with the same own RuntimeDirectory complete
			// the command's whole handoff; rejecting all node runtime paths fails.
			writeFile(t, f.cfg, original, 0o600)
			mustOK(t, os.Chtimes(f.cfg, info.ModTime(), info.ModTime()))
			out, code = f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			retiredAnswer(t, out, code)
			if !slices.Contains(f.manager.operations, "stop "+nodeUnit) || !slices.Contains(f.manager.operations, "start "+nodeUnit) {
				t.Fatalf("persistent control did not complete the node handoff: %v", f.manager.operations)
			}
		})
	}
}

func TestRetirementReprovesVolatileInputsAtStoppedAndArchiveBoundaries(t *testing.T) {
	for _, boundary := range []string{"status", "journal", "archive"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			root := t.TempDir()
			key, entry := filepath.Join(root, "key"), filepath.Join(root, "entry")
			writeFile(t, key, "retained key", 0o600)
			mustOK(t, os.Symlink(key, entry))
			resource, err := observeRetireResource(entry)
			mustOK(t, err)
			f.originalNode.Resources = append(f.originalNode.Resources, resource)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			mustOK(t, retirement.WriteStatus(j.Phase, j.Variant, retireNow()))
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy remaining sequence: %+v", r)
			}
			bridge := filepath.Join(f.unitsDir, "volatile", "run", "tls")
			mustOK(t, os.MkdirAll(filepath.Dir(bridge), 0o700))
			mustOK(t, os.Symlink(key, bridge))
			introduced := false
			move := func() {
				if introduced {
					return
				}
				introduced = true
				mustOK(t, os.Remove(entry))
				mustOK(t, os.Symlink(bridge, entry))
				// The final path, inode and file remain identical; only traversal
				// through a volatile intermediate link changes.
				now, err := observeRetireResource(entry)
				mustOK(t, err)
				if now != resource {
					t.Fatal("boundary fixture changed the captured resource identity")
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
					t.Fatalf("healthy stopped publication: %+v", r)
				}
				j = next
				next, r = retireArchive(t.Context(), j)
			}
			if !introduced || r == nil || r.Reason != retireReasonStopped || !strings.Contains(r.Why, "retained-input-volatile") || next.Phase != j.Phase {
				t.Fatalf("boundary admitted volatile traversal: phase=%s refusal=%+v", next.Phase, r)
			}
			status, _, err := retirement.ReadStatus()
			mustOK(t, err)
			want := retirement.PhaseStopped
			if boundary == "status" {
				want = retirement.PhaseIntent
			}
			if status.Phase != want || requireRetireJournal(t).Phase != j.Phase {
				t.Fatal("volatile traversal advanced publication")
			}
			if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
				t.Fatalf("volatile traversal crossed archive rename: %v", err)
			}
		})
	}
}

func TestRetirementCapturesOnlyRecreatedRecordsAsDisposable(t *testing.T) {
	f := newRequestFixture(t)
	f.retainANode(t)
	installed, r := observeRetireConfig(f.cfg)
	if r != nil {
		t.Fatal(r)
	}
	want := f.originalNode
	p := retireOperationProtection(retirement.Journal{RetainedInvocation: want})
	if want.ConfigPath != f.cfg || !slices.Contains(p.RequiredInputs[nodeUnit], f.cfg) {
		t.Fatal("captured configuration is not a required input")
	}
	for _, path := range []string{installed.cfg.Node.TLS.CertPath, installed.cfg.Node.TLS.KeyPath, installed.cfg.Node.TLS.CAPath, installed.cfg.Node.StateDir} {
		if !slices.Contains(p.RequiredInputs[nodeUnit], path) || slices.Contains(p.UnitPaths[nodeUnit], path) {
			t.Fatalf("required input classified as disposable: %s", path)
		}
	}
	for _, resource := range want.Resources {
		disposable := resource.Path == filepath.Dir(registrationRecordPath) || resource.Path == installed.cfg.Node.LockDir
		if resource.Runtime != disposable {
			t.Fatalf("wrong captured resource class for %s: runtime=%v", resource.Path, resource.Runtime)
		}
		if disposable && !slices.Contains(p.UnitPaths[nodeUnit], resource.Path) {
			t.Fatalf("recreated record lost its ownership: %s", resource.Path)
		}
	}
}
