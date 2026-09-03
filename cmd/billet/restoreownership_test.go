package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/state"
)

// The ownership half of a restore, which nothing checked until the rehearsal ran
// one on a real packaged Linux host.
//
// A restore there runs as ROOT — it has to, because the App key lands in
// root-owned /etc/billet — and everything it publishes is then root-owned inside
// a state directory the service account owns. systemd's StateDirectory= does not
// repair that (it walks the tree only when the TOP directory's owner is wrong),
// and `billet local up`'s repair names the five files a preflight creates and no
// part of the authority. The deployment restored perfectly and the control plane
// could not open a single file of it.

// asRootOnLinux makes this process look like a privileged restore on a packaged
// host, and hands back the converger it will find.
func asRootOnLinux(t *testing.T) *fakeConverger {
	t.Helper()

	prevOS, prevUID, prevConverge := hostOS, geteuid, converge

	t.Cleanup(func() { hostOS, geteuid, converge = prevOS, prevUID, prevConverge })

	f := &fakeConverger{}
	hostOS = "linux"
	geteuid = func() int { return 0 }
	converge = func(...lifeops.ConvergeOption) converger { return f }

	return f
}

// repairedNames pulls the entry names out of the fake's record of the call.
func repairedNames(t *testing.T, f *fakeConverger, stateDir string) []string {
	t.Helper()

	prefix := "repair-paths " + stateDir + " "

	for _, line := range f.trace {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.Split(rest, ",")
		}
	}

	t.Fatalf("nothing repaired %s; the fake recorded %v", stateDir, f.trace)

	return nil
}

// TestARootRestoreHandsTheWholeDeploymentToTheServiceAccount drives the
// COMMAND, not the helper.
//
// The property is that `billet local restore` repairs what it wrote — proving
// the repair function works in isolation would say nothing about whether the
// restore ever calls it, which is the shape three defects in this repository
// have already had.
func TestARootRestoreHandsTheWholeDeploymentToTheServiceAccount(t *testing.T) {
	stubLifecycleLock(t)

	f := asRootOnLinux(t)

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v", err)
	}

	got := repairedNames(t, f, tgt.stateDir)

	// EVERY ONE OF THESE IS A FILE THE CONTROL PLANE OPENS. The authority was the
	// half that was missing: a root-owned 0700 ca/ cannot even be traversed, so
	// the two files under it are unreachable however they are owned.
	for _, want := range []string{
		"deployment-id",
		"billet.db",
		"billet.db-wal",
		"billet.db-shm",
		"billet.lock",
		"ca.lock",
		"ca/",
		"ca/ca.key",
		"ca/ca.crt",
		"authority-created",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("a root restore did not hand over %q; it handed over %v", want, got)
		}
	}

	// AND THE APP KEY, which lives outside the state directory and so cannot be
	// reached by the confined walk above.
	var named bool

	for _, change := range f.ownershipChanges {
		if change.Path == tgt.keyPath {
			named = true

			if change.Mode.Perm() != 0o600 {
				t.Errorf("the App key was handed over as mode %04o; billet refuses any key "+
					"readable beyond its owner", change.Mode.Perm())
			}
		}
	}

	if !named {
		t.Errorf("a root restore left %s owned by root; the service cannot read it and billet "+
			"refuses to start without it", tgt.keyPath)
	}
}

// An unprivileged restore changes no ownership at all.
//
// A restore run AS the service account already owns everything it wrote, and a
// chown from an unprivileged process fails with EPERM — so attempting one turns
// a correct restore into a report of a failure that is not one.
func TestARestoreThatIsNotRootChangesNoOwnership(t *testing.T) {
	stubLifecycleLock(t)

	f := asRootOnLinux(t)
	geteuid = func() int { return 1000 }

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("billet local restore: %v", err)
	}

	if len(f.trace) > 0 {
		t.Errorf("an unprivileged restore touched ownership: %v", f.trace)
	}
}

// A failed ownership repair does NOT fail the restore.
//
// THE RESTORE IS FINISHED AND CORRECT BY THEN. Every credential is in place and
// verified, so returning an error would read as "the restore failed" on the
// worst day of a deployment's life — and the next thing an operator reaches for
// is --abandon, which DELETES what this run installed.
func TestAFailedOwnershipRepairDoesNotFailTheRestore(t *testing.T) {
	stubLifecycleLock(t)

	f := asRootOnLinux(t)
	f.repairPathsErr = errors.New("operation not permitted")

	src := newBackupFixture(t, true)
	archive := backupInto(t, src)

	tgt := newBackupFixture(t, false)

	clearAppKey(t, tgt)

	if err := cmdLocalRestore(t.Context(), []string{
		"--config", tgt.configPath, "--from", archive, "--old-controller-fenced",
	}); err != nil {
		t.Fatalf("a restore that published everything reported failure over ownership: %v", err)
	}

	// AND THE FAILING PATH WAS GENUINELY REACHED. Without this the test passes
	// against a restore that never attempted a repair at all — the same green for
	// the opposite reason, which is the trap this repository keeps finding.
	repairedNames(t, f, tgt.stateDir)

	// And the deployment really is there, which is the fact that makes exiting 0
	// honest rather than convenient.
	id, found, err := state.PeekDeploymentID(tgt.stateDir)
	if err != nil || !found {
		t.Fatalf("PeekDeploymentID: %v (found=%v)", err, found)
	}

	if id != src.deployment {
		t.Errorf("restored deployment %s, want %s", id, src.deployment)
	}
}

// The repair set never leaves the state directory.
//
// The walk is confined to the state directory, so a path outside it — the App
// key, which the planner REFUSES to place inside — must not be handed to it. A
// name that escaped would be silently dropped by os.Root, which reads exactly
// like a file that did not need repairing.
func TestRestoredStateTargetsNeverLeaveTheStateDirectory(t *testing.T) {
	stateDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "app-private-key.pem")

	targets := restoredStateTargets(deployarchive.Plan{
		Target: deployarchive.Target{StateDir: stateDir, AppKeyPath: outside},
		Actions: []deployarchive.Action{
			{Entry: deployarchive.EntryLedger, Path: filepath.Join(stateDir, "billet.db")},
			{Entry: deployarchive.EntryAppKey, Path: outside},
		},
	})

	for _, target := range targets {
		if strings.HasPrefix(target.Name, "..") || filepath.IsAbs(target.Name) {
			t.Errorf("%q escapes the state directory", target.Name)
		}
	}

	if len(targets) == 0 {
		t.Fatal("no targets at all, so the check above proved nothing")
	}
}
