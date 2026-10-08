package hostauthority

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

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
		acc, err := Open(t.Context(), dir, Intent{Wait: time.Second})
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
		for _, set := range []ArtefactSet{IdentityArtefacts, LedgerArtefacts} {
			targets, err := ArtefactTargets(dir, set)
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
