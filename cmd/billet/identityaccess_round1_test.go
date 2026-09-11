package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/wirecert"
)

// preparedHost records a service account and creates the global lock under the
// pinned root, the metadata an installer leaves; the account is this test's
// own uid so an unprivileged run can open what it created.
func preparedHost(t *testing.T) retirement.ServiceAccount {
	t.Helper()

	acct := retirement.ServiceAccount{User: "billet", UID: os.Getuid(), Group: "billet", GID: os.Getgid()}
	if acct.UID == 0 {
		acct.UID, acct.GID = 1000, 1000
	}

	if err := retirement.WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	hold, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true, Account: &acct})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	return acct
}

// AN ABSENT DIRECTORY ON A PREPARED HOST IS NEVER RECREATED BY AN ORDINARY
// COMMAND: on a prepared host the directory exists from the installer's
// bootstrap onwards and is moved only by a retirement, so its absence is that
// move (or damage), and a command that recreated it would mint a second
// identity beside the archived one. The refusal creates nothing and releases
// the global lock it took to look.
func TestAPreparedHostsAbsentDirectoryIsNeverRecreated(t *testing.T) {
	useRetirementRoot(t)
	preparedHost(t)

	dir := filepath.Join(t.TempDir(), "server")

	for _, intent := range []identityIntent{{create: true, wait: time.Second}, {wait: time.Second}} {
		acc, err := openIdentityAccess(t.Context(), dir, intent)
		if err == nil {
			t.Fatal(errors.Join(fmt.Errorf("intent %+v: an absent directory on a prepared host must refuse", intent), acc.Release()))
		}

		if !strings.Contains(err.Error(), "will not recreate") {
			t.Errorf("the refusal must say the directory is not recreated, got %v", err)
		}
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refusal must create nothing")
	}

	if locked(t, retirement.GlobalLockPath()) {
		t.Fatal("the refusal must release the global lock")
	}

	// THE LEDGER FACTORY TAKES THE SAME EXCLUSION when no command holds one, so
	// its opener never reaches the absent directory: refused, nothing created,
	// the locks released.
	cfg, err := config.Load(writeCAConfig(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	db, err := openStateAdmin(t.Context(), cfg)
	if err == nil {
		t.Fatal(errors.Join(errors.New("the ledger open on a prepared host recreated the absent directory"), db.Close()))
	}

	if !strings.Contains(err.Error(), "will not recreate") {
		t.Errorf("the factory's refusal must be the exclusion's, got %v", err)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the factory's refusal must create nothing")
	}

	if locked(t, retirement.GlobalLockPath()) {
		t.Fatal("the factory must release the global lock it took")
	}
}

// THE LEDGER FACTORY BORROWS A HELD ACCESS AND TAKES ITS OWN OTHERWISE: with
// the command's access open the factory opens under it (a second take in one
// process would be denied), and without one it takes and releases the inner
// lock around the open, so nothing is left held behind the handle.
func TestTheLedgerFactoryBorrowsAHeldAccessAndTakesItsOwnOtherwise(t *testing.T) {
	useRetirementRoot(t)

	dir := t.TempDir()
	cfg, err := config.Load(writeCAConfig(t, dir))
	if err != nil {
		t.Fatal(err)
	}

	acc, err := openIdentityAccess(t.Context(), dir, identityIntent{wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if !identityAccessHeld(dir) || !identityAccessHeld(dir+"/") {
		t.Fatal("an open access must be registered for its directory, however it is spelled")
	}

	db, err := openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("the factory must borrow the held access, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if !locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("a borrowed access is not released by the factory")
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}

	if identityAccessHeld(dir) {
		t.Fatal("a released access must be unregistered")
	}

	db, err = openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("the factory must take its own access when none is held, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("the factory's own access must be released once the handle exists")
	}

	// THE FACTORY'S OWN TAKE IS AN EXCLUSION, PROVED: with the inner lock held
	// by a raw lock the registry knows nothing of, the factory cannot open
	// until its bound, and opens once the lock is released. A presence check
	// in place of the exclusion would open at once.
	oldWait := identityAccessWait
	identityAccessWait = 300 * time.Millisecond

	t.Cleanup(func() { identityAccessWait = oldWait })

	raw, err := wirecert.LockAuthority(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now()

	db, err = openStateAdmin(t.Context(), cfg)
	if err == nil {
		t.Fatal(errors.Join(errors.New("the factory opened the ledger while the inner lock was held elsewhere"), db.Close()))
	}

	if time.Since(started) < 250*time.Millisecond {
		t.Fatalf("the factory refused at once (%v) rather than waiting for the held lock", time.Since(started))
	}

	if err := raw.Release(); err != nil {
		t.Fatal(err)
	}

	db, err = openStateAdmin(t.Context(), cfg)
	if err != nil {
		t.Fatalf("the factory must open once the lock is released, got %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// A FIFO AT THE INNER LOCK'S NAME IS REFUSED PROMPTLY, not opened and waited on:
// the inner lock goes through the same regular-file rule as the global one.
func TestAFIFOAtTheInnerLockRefusesWithoutWaiting(t *testing.T) {
	useRetirementRoot(t)

	dir := t.TempDir()
	if err := syscall.Mkfifo(wirecert.AuthorityLockPath(dir), 0o600); err != nil {
		t.Fatal(err)
	}

	answered := make(chan error, 1)

	go func() {
		acc, err := openIdentityAccess(t.Context(), dir, identityIntent{wait: time.Second})
		if err == nil {
			err = errors.Join(errors.New("a FIFO at ca.lock was accepted as the lock"), acc.Release())
		}

		answered <- err
	}()

	select {
	case err := <-answered:
		if err == nil || errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "accepted") {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the open of a FIFO blocked the acquisition")
	}
}

// THE ARTEFACT NAMES ARE RELATIVE TO THE CLEANED DIRECTORY, whatever spelling
// the configuration used: the producers' helpers clean their paths, and a
// directory given with a trailing slash once left every target absolute, which
// the repair's root refuses after the command has already created them.
func TestArtefactNamesAreRelativeWhateverTheDirectorysSpelling(t *testing.T) {
	for _, dir := range []string{"/var/lib/billet/server", "/var/lib/billet/server/", "/var/lib/billet//server/./"} {
		for _, set := range []artefactSet{identityArtefacts, ledgerArtefacts} {
			targets, err := artefactTargets(dir, set)
			if err != nil {
				t.Fatalf("%q: %v", dir, err)
			}

			for _, target := range targets {
				if filepath.IsAbs(target.Name) || strings.HasPrefix(target.Name, "..") {
					t.Errorf("%q: target %q is not a relative name inside the directory", dir, target.Name)
				}
			}
		}
	}
}
