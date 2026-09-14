package retirement

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTheHostIsClassifiedFromFourObservationsAndDamageNeverDowngrades(t *testing.T) {
	root := useRoot(t)
	identity := filepath.Join(root, "server")

	c, err := Classify(identity)
	if err != nil || c.Mode != ModeFresh {
		t.Fatalf("nothing at all is a fresh host, got %v %v", c, err)
	}

	if err := os.Mkdir(identity, 0o700); err != nil {
		t.Fatal(err)
	}

	c, err = Classify(identity)
	if err != nil || c.Mode != ModeLegacy {
		t.Fatalf("a directory with no metadata is the legacy path, got %v %v", c, err)
	}

	// A global lock beside no record is damage, whatever the directory says.
	hold, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	var damaged DamagedError
	if _, err := Classify(identity); !errors.As(err, &damaged) || damaged.Why == "" {
		t.Fatalf("a global lock without a record is damage with a reason, got %v", err)
	}

	if err := os.Remove(GlobalLockPath()); err != nil {
		t.Fatal(err)
	}

	if err := WriteStatus(PhaseStopped, VariantServerOnly, time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, err := Classify(identity); !errors.As(err, &damaged) {
		t.Fatalf("a status without a record is damage, got %v", err)
	}

	if err := os.Remove(StatusPath()); err != nil {
		t.Fatal(err)
	}

	acct := ServiceAccount{User: "ci", UID: 1234, Group: "ci", GID: 1234}
	if err := WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	c, err = Classify(identity)
	if err != nil || c.Mode != ModePrepared || c.Account != acct {
		t.Fatalf("a record makes the host prepared, got %v %v", c, err)
	}

	// An unreadable record is damage, never absence.
	if err := os.WriteFile(ServiceAccountPath(), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Classify(identity); !errors.As(err, &damaged) {
		t.Fatalf("an unreadable record is damage, got %v", err)
	}
}

// The privileged operations refuse a caller that is not root, and prove their
// ownership work in the lifecycle job's real container; here they are held to
// the refusal, and the lock repair to what a non-root caller can observe.
func TestBootstrapAndTheLockRepairAreRootsAlone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the refusal is for a caller that is not root")
	}

	useRoot(t)

	acct := ServiceAccount{User: "billet", UID: 998, Group: "billet", GID: 997}

	if _, err := Bootstrap(t.Context(), BootstrapRequest{
		Account: acct, IdentityDir: filepath.Join(Root, "server"),
	}); !errors.Is(err, ErrNotPrivileged) {
		t.Fatalf("bootstrap: got %v", err)
	}

	if _, err := RepairLocks(acct, ""); !errors.Is(err, ErrNotPrivileged) {
		t.Fatalf("repair: got %v", err)
	}

	if _, err := os.Lstat(ServiceAccountPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused bootstrap must write nothing")
	}
}

func TestReowningALockKeepsItsInodeAndRefusesALink(t *testing.T) {
	root := useRoot(t)

	hold, err := Acquire(t.Context(), AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	releaseLater(t, hold.Release)

	before, err := os.Stat(GlobalLockPath())
	if err != nil {
		t.Fatal(err)
	}

	// The one re-owning a non-root caller may make is to itself; what is
	// observable is the mode change, the inode kept and the hold surviving.
	changed, err := reownLock(GlobalLockPath(), os.Getuid(), os.Getgid(), 0o660)
	if err != nil || !changed {
		t.Fatalf("got %v %v", changed, err)
	}

	after, err := os.Stat(GlobalLockPath())
	if err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(before, after) || after.Mode().Perm() != 0o660 {
		t.Fatalf("the repair must keep the inode and set the mode, got same=%v mode=%v",
			os.SameFile(before, after), after.Mode())
	}

	if changed, err := reownLock(GlobalLockPath(), os.Getuid(), os.Getgid(), 0o660); err != nil || changed {
		t.Fatalf("a second repair changes nothing, got %v %v", changed, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	if _, err := Acquire(ctx, AcquireOptions{Poll: 10 * time.Millisecond}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the hold must survive the repair, got %v", err)
	}

	target := filepath.Join(root, "ca.lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "link.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if _, err := reownLock(link, os.Getuid(), os.Getgid(), 0o600); err == nil {
		t.Fatal("a symlink at a lock's name must refuse")
	}

	if changed, err := reownLock(filepath.Join(root, "absent.lock"), os.Getuid(), os.Getgid(), 0o600); err != nil || changed {
		t.Fatalf("an absent lock is left absent, got %v %v", changed, err)
	}
}
