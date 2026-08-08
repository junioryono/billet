package state

import (
	"os"
	"path/filepath"
	"testing"
)

// The identity is stable across calls, because everything billet has already
// started is labelled with the previous answer.
func TestDeploymentIDIsStable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	second, err := DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID again: %v", err)
	}

	if first != second {
		t.Errorf("identity changed between calls: %q then %q — every container "+
			"labelled with the first would become invisible", first, second)
	}
}

// TWO STATE DIRECTORIES ARE TWO INSTALLATIONS, and this is the whole reason the
// identity exists.
//
// The process lock guards a directory, so two billets with different state
// directories both start happily. Labelling their compute by node name would
// then give them the same label — hostnames match, since it is one machine — and
// the first to reconcile would find the other's lease ids absent from its own
// database and destroy live jobs it has no relationship with.
func TestTwoStateDirectoriesGetDifferentIdentities(t *testing.T) {
	t.Parallel()

	a, err := DeploymentID(t.TempDir())
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	b, err := DeploymentID(t.TempDir())
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	if a == b {
		t.Fatal("two installations share an identity, so each can destroy the other's containers")
	}
}

func TestDeploymentIDIsCreatedPrivate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := DeploymentID(dir); err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, deploymentIDFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("deployment id is mode %o, want 600", perm)
	}
}

// An empty file is refused rather than treated as "no identity yet".
//
// Minting a fresh one would be the friendlier-looking choice and the wrong one:
// the empty file means a previous write was interrupted, and anything that run
// started is labelled with an id nothing can now reproduce. Failing says so.
func TestAnEmptyDeploymentIDIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.WriteFile(filepath.Join(dir, deploymentIDFile), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := DeploymentID(dir); err == nil {
		t.Fatal("an empty identity file was accepted, so compute would be labelled with nothing")
	}
}

// A copied state directory carries the original's identity.
//
// Deliberate: the copy IS the same installation as far as its containers are
// concerned, and minting a new id for it would strand every one of them.
//
// THE EARLIER VERSION OF THIS COMMENT CLAIMED THE DIRECTORY LOCK MAKES THAT SAFE.
// It does not. That lock is taken on a file INSIDE the state directory, and a COPY
// has its own inode, so both directories can be locked at once — two live processes
// claiming one identity against one docker daemon, each able to act on the other's
// containers.
//
// What closes it is a second lock keyed by the identity rather than by a path:
// LockDeployment, exercised end to end by
// TestACopiedStateDirectoryCannotRunAlongsideTheOriginal. This test is the half
// that lock DEPENDS on — that the copy keeps the identity in the first place.
// A copy that minted a fresh one would strand every container the original
// started, and would never collide either.
func TestACopiedStateDirectoryKeepsItsIdentity(t *testing.T) {
	t.Parallel()

	original := t.TempDir()

	want, err := DeploymentID(original)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(original, deploymentIDFile))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	copied := t.TempDir()

	if err := os.WriteFile(filepath.Join(copied, deploymentIDFile), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := DeploymentID(copied)
	if err != nil {
		t.Fatalf("DeploymentID of the copy: %v", err)
	}

	if got != want {
		t.Errorf("the copy minted a new identity (%q, was %q), stranding every container "+
			"the original started", got, want)
	}
}
