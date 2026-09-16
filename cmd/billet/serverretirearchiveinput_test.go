package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

func TestRetirementRefusesConfigurationInsideTheArchiveBeforeIntent(t *testing.T) {
	for _, spelling := range []string{"direct", "parent symlink"} {
		t.Run(spelling, func(t *testing.T) {
			f := newRequestFixture(t)
			retainAndRestartANode(t, f)
			f.reserve(t)
			body := mustRead(t, f.cfg)
			// Model /var/lib/billet/server and /etc/billet on the fixture host.
			path := filepath.Join(f.stateDir, "billet.yaml")
			if spelling == "parent symlink" {
				target := filepath.Join(f.stateDir, "etc")
				mustOK(t, os.MkdirAll(target, 0o700))
				parent := filepath.Join(t.TempDir(), "billet")
				mustOK(t, os.Symlink(target, parent))
				path = filepath.Join(parent, "billet.yaml")
			}
			writeFile(t, path, body, 0o600)
			f.cfg = path
			setRetireNodeCommand(t, f, []string{installedBinary, "node", "--config", path}, "")
			setRetireEffect(t, f, serverUnit, "StateDirectory", "")
			out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
			if code != exitUnknown || !strings.Contains(out, "retained-input-archived") {
				t.Fatalf("archive-dependent configuration admitted: %s", out)
			}
			if len(f.manager.operations) != 0 || mustRead(t, path) != body {
				t.Fatalf("archive-dependent configuration crossed a mutation: %v", f.manager.operations)
			}
			if _, presence, err := retirement.ReadJournal(); err != nil || presence != retirement.JournalAbsent {
				t.Fatalf("archive-dependent configuration published intent: %v %v", presence, err)
			}
			if _, err := os.Stat(f.stateDir); err != nil {
				t.Fatalf("identity directory renamed before refusal: %v", err)
			}
		})
	}
}

func TestRetirementKeepsConfigurationIdentityUntilItsAuthorizedRewrite(t *testing.T) {
	f := newRequestFixture(t)
	persistent := filepath.Join(t.TempDir(), "etc", "billet", "billet.yaml")
	mustOK(t, os.MkdirAll(filepath.Dir(persistent), 0o700))
	writeFile(t, persistent, mustRead(t, f.cfg), 0o600)
	f.cfg = persistent
	retainAndRestartANode(t, f)
	f.reserve(t)
	original, err := observeRetireResource(f.cfg)
	mustOK(t, err)
	beforeArchive, afterArchive, beforeRewrite := false, false, false
	check := func() {
		now, err := observeRetireResource(f.cfg)
		mustOK(t, err)
		if now != original {
			t.Fatal("persistent configuration identity changed before its authorized rewrite")
		}
	}
	savedRename, savedConfig, savedSync := retireBeforeRename, retireBeforeConfigRename, retireSyncDir
	retireBeforeRename = func() { beforeArchive = true; check() }
	retireBeforeConfigRename = func() { beforeRewrite = true; check() }
	retireSyncDir = func(dir string) error {
		if beforeArchive && !beforeRewrite {
			if _, err := os.Lstat(f.stateDir); os.IsNotExist(err) {
				afterArchive = true
				check()
			}
		}
		return savedSync(dir)
	}
	t.Cleanup(func() {
		retireBeforeRename, retireBeforeConfigRename, retireSyncDir = savedRename, savedConfig, savedSync
	})
	out, code := f.retainedRequest(t, f.input(t, f.retainedOverrides(t)))
	retiredAnswer(t, out, code)
	if !beforeArchive || !afterArchive || !beforeRewrite {
		t.Fatalf("control missed archive/rewrite boundaries: %v %v %v", beforeArchive, afterArchive, beforeRewrite)
	}
	j := requireRetireJournal(t)
	if !slices.Contains(j.RetainedInvocation.Resources, original) {
		t.Fatal("journal omitted the original configuration resource identity")
	}
	now, err := observeRetireResource(f.cfg)
	mustOK(t, err)
	if j.RetainedInvocation.ConfigReplacement == nil || *j.RetainedInvocation.ConfigReplacement != now || now == original {
		t.Fatal("rewrite did not record its exact replacement identity")
	}
}

func TestRetirementRechecksArchiveTraversalAtEveryStoppedBoundary(t *testing.T) {
	for _, boundary := range []string{"status", "journal", "archive"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			key := filepath.Join(t.TempDir(), "node.env")
			entry := filepath.Join(t.TempDir(), "node.env")
			writeFile(t, key, "", 0o600)
			mustOK(t, os.Symlink(key, entry))
			setRetireEffect(t, f, nodeUnit, "EnvironmentFiles", entry+" (ignore_errors=no)")
			installed, r := observeRetireConfig(f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			f.originalNode, r = captureRetireInvocation(t.Context(), installed.cfg, f.cfg)
			if r != nil {
				t.Fatal(r)
			}
			resource, err := observeRetireResource(entry)
			mustOK(t, err)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			mustOK(t, retirement.WriteStatus(j.Phase, j.Variant, retireNow()))
			if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
				t.Fatalf("healthy remaining sequence: %+v", r)
			}
			bridge := filepath.Join(f.stateDir, "node.env")
			mustOK(t, os.Symlink(key, bridge))
			introduced := false
			move := func() {
				if introduced {
					return
				}
				introduced = true
				mustOK(t, os.Remove(entry))
				mustOK(t, os.Symlink(bridge, entry))
				now, err := observeRetireResource(entry)
				mustOK(t, err)
				if now != resource {
					t.Fatal("traversal fixture changed the final resource")
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
					t.Fatalf("healthy stopped control: %+v", r)
				}
				j = next
				next, r = retireArchive(t.Context(), j)
			}
			if !introduced || r == nil || r.Reason != retireReasonStopped || !strings.Contains(r.Why, "retained-input-archived") || next.Phase != j.Phase {
				t.Fatalf("archive traversal passed boundary: %+v %s", r, next.Phase)
			}
			if _, err := os.Lstat(j.Archive); !os.IsNotExist(err) {
				t.Fatalf("archive traversal crossed rename: %v", err)
			}
			status, _, err := retirement.ReadStatus()
			mustOK(t, err)
			want := retirement.PhaseStopped
			if boundary == "status" {
				want = retirement.PhaseIntent
			}
			if status.Phase != want || requireRetireJournal(t).Phase != j.Phase {
				t.Fatal("archive traversal advanced publication")
			}
		})
	}
}

func TestRetirementComparesConfigurationIdentityAfterArchiveAndBeforeRewrite(t *testing.T) {
	for _, boundary := range []string{"archive flush", "rewrite flush"} {
		t.Run(boundary, func(t *testing.T) {
			f := newRequestFixture(t)
			f.retainANode(t)
			f.reserve(t)
			j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
			j, r := retireStop(t.Context(), j)
			if r != nil {
				t.Fatal(r)
			}
			changed := false
			replace := func() {
				if changed {
					return
				}
				changed = true
				info, err := os.Stat(f.cfg)
				mustOK(t, err)
				writeFile(t, f.cfg+".replacement", mustRead(t, f.cfg), 0o600)
				// Keep the phase table's unchanged-config fact; only inode identity
				// distinguishes this unapproved replacement from the original.
				mustOK(t, os.Chtimes(f.cfg+".replacement", info.ModTime(), info.ModTime()))
				mustOK(t, os.Rename(f.cfg+".replacement", f.cfg))
			}
			savedSync, savedConfig := retireSyncDir, retireBeforeConfigRename
			retireSyncDir = func(dir string) error {
				if boundary == "archive flush" {
					replace()
				}
				return savedSync(dir)
			}
			retireBeforeConfigRename = replace
			t.Cleanup(func() { retireSyncDir, retireBeforeConfigRename = savedSync, savedConfig })
			next, r := retireArchive(t.Context(), j)
			if boundary == "rewrite flush" {
				if r != nil {
					t.Fatal(r)
				}
				j = next
				next, r = retireRewrite(t.Context(), retireProofMode(f), nil, j)
			}
			if !changed || r == nil || !strings.Contains(r.Why, "retained node resource changed: "+f.cfg) || next.Phase != j.Phase {
				t.Fatalf("unapproved config replacement crossed boundary: %+v %s", r, next.Phase)
			}
			if requireRetireJournal(t).Phase != j.Phase || slices.Contains(f.manager.operations, "stop "+nodeUnit) {
				t.Fatal("unapproved configuration identity advanced the handoff")
			}
		})
	}
}

func TestRetirementResumesOnlyItsRecordedConfigurationReplacement(t *testing.T) {
	f := newRequestFixture(t)
	retainAndRestartANode(t, f)
	f.reserve(t)
	j := f.plantJournal(t, retirement.PhaseIntent, retirement.VariantRetainedNode)
	j, r := retireStop(t.Context(), j)
	if r != nil {
		t.Fatal(r)
	}
	j, r = retireArchive(t.Context(), j)
	if r != nil {
		t.Fatal(r)
	}
	savedSync := retireSyncDir
	retireSyncDir = func(string) error { return errors.New("fixture interrupted after config rename") }
	t.Cleanup(func() { retireSyncDir = savedSync })
	next, r := retireRewrite(t.Context(), retireProofMode(f), nil, j)
	if r == nil || !strings.Contains(r.Why, "fixture interrupted") || next.Phase != retirement.PhaseArchived {
		t.Fatalf("fixture missed rewrite crash window: %+v %s", r, next.Phase)
	}
	retireSyncDir = savedSync
	j = requireRetireJournal(t)
	if j.RetainedInvocation.ConfigReplacement == nil {
		t.Fatal("rewrite did not persist replacement identity before rename")
	}
	if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r != nil {
		t.Fatalf("recorded rewrite could not resume: %+v", r)
	}
	writeFile(t, f.cfg+".foreign", mustRead(t, f.cfg), 0o600)
	mustOK(t, os.Rename(f.cfg+".foreign", f.cfg))
	if r := admitRetireRemaining(t.Context(), retireProofMode(f), j); r == nil || !strings.Contains(r.Why, "retained node resource changed: "+f.cfg) {
		t.Fatalf("same digest on a foreign inode adopted as the recorded rewrite: %+v", r)
	}
}
