package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE EXCLUSION BEFORE THE FIRST IDENTITY ACCESS, PROVED WHERE A NON-ROOT TEST
// CAN PROVE IT. The ownership hand-backs and the package's bootstrap need root
// and a real service account; the lifecycle job's container runs them. What
// this file holds to is the classification, the locks each mode takes and in
// which order, and the refusals: an absent directory a command may not create,
// a closed authority, metadata that contradicts itself.

// useRetirementRoot points the leaf at a directory the test owns and pins the
// platform seam to Linux, since the exclusion is Linux's.
func useRetirementRoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	oldRoot := retirement.Root
	retirement.Root = root

	oldOS, oldPlatform := hostOS, retirement.Platform
	hostOS, retirement.Platform = "linux", "linux"

	t.Cleanup(func() {
		retirement.Root = oldRoot
		hostOS, retirement.Platform = oldOS, oldPlatform
	})

	return root
}

// locked reports whether path's flock is held by somebody else, by trying a
// non-blocking take on a fresh descriptor and releasing it at once.
func locked(t *testing.T, path string) bool {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}

		t.Fatal(err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}

		return false
	}

	return true
}

func TestAFreshDirectoryIsCreatedUnderTheInitLockAndTheInnerLockTakenInsideIt(t *testing.T) {
	useRetirementRoot(t)

	parent := t.TempDir()
	dir := filepath.Join(parent, "server")

	// A command that creates nothing refuses a directory that is not there.
	if _, err := openIdentityAccess(t.Context(), dir, identityIntent{}); err == nil ||
		!strings.Contains(err.Error(), "creates nothing") {
		t.Fatalf("an absent directory must refuse a command that may not create it, got %v", err)
	}

	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the refusal must create nothing")
	}

	acc, err := openIdentityAccess(t.Context(), dir, identityIntent{create: true, wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if !acc.created {
		t.Fatal("the fresh branch must report that it created the directory")
	}

	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the directory must exist at 0700, got %v %v", info, err)
	}

	if !locked(t, retirement.InitLockPath(dir)) {
		t.Fatal("the init lock beside the directory must be held while the access is open")
	}

	if !locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("the inner lock inside the created directory must be held")
	}

	if acc.Account() != nil {
		t.Fatal("a fresh host has no recorded account")
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}

	if locked(t, retirement.InitLockPath(dir)) || locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("release must drop both locks")
	}
}

func TestTheLegacyPathTakesTheInnerLockAndExcludesASecondCommand(t *testing.T) {
	useRetirementRoot(t)

	dir := filepath.Join(t.TempDir(), "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	acc, err := openIdentityAccess(t.Context(), dir, identityIntent{})
	if err != nil {
		t.Fatal(err)
	}

	defer func() {
		if err := acc.Release(); err != nil {
			t.Error(err)
		}
	}()

	if acc.created || acc.Account() != nil {
		t.Fatal("an existing directory with no metadata is the legacy path: nothing created, no account")
	}

	if !locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("the legacy path must hold the inner lock")
	}

	if _, err := os.Lstat(retirement.GlobalLockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the legacy path must not create the global lock")
	}

	// A second command on the same directory is told what holds the lock rather
	// than queued behind it: the operator command's non-blocking take.
	if _, err := openIdentityAccess(t.Context(), dir, identityIntent{}); err == nil ||
		!strings.Contains(err.Error(), "another billet is working on this deployment's certificate authority") {
		t.Fatalf("a held inner lock must refuse a second command by name, got %v", err)
	}

	// And the wirecert entry points that require the lock held refuse a nil one.
	if _, err := wirecert.RotateWith(nil, dir, "deadbeef"); err == nil {
		t.Fatal("RotateWith without a lock must refuse")
	}

	if err := wirecert.RetireWith(nil, dir, "deadbeef"); err == nil {
		t.Fatal("RetireWith without a lock must refuse")
	}
}

func TestAPreparedHostTakesTheGlobalLockFirstAndAClosedStatusRefuses(t *testing.T) {
	useRetirementRoot(t)

	dir := filepath.Join(t.TempDir(), "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// An installer's publication, as far as a non-root test can make it: the
	// record and the global lock file (created by the test's own uid).
	acct := retirement.ServiceAccount{User: "billet", UID: os.Getuid(), Group: "billet", GID: os.Getgid()}
	if err := retirement.WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	hold, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	acc, err := openIdentityAccess(t.Context(), dir, identityIntent{wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	if acc.Account() == nil || acc.Account().UID != os.Getuid() {
		t.Fatalf("a prepared host carries its recorded account, got %+v", acc.Account())
	}

	if !locked(t, retirement.GlobalLockPath()) || !locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("a prepared host holds the global lock and the inner lock together")
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}

	if locked(t, retirement.GlobalLockPath()) || locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("release must drop both")
	}

	// A closed authority refuses every ordinary command, naming its phase.
	if err := retirement.WriteStatus(retirement.PhaseArchived, retirement.VariantServerOnly, time.Now()); err != nil {
		t.Fatal(err)
	}

	var retiring retirement.ErrRetiring
	if _, err := openIdentityAccess(t.Context(), dir, identityIntent{wait: time.Second}); !errors.As(err, &retiring) ||
		retiring.Phase != retirement.PhaseArchived {
		t.Fatalf("a closed status must refuse naming the phase, got %v", err)
	}

	if locked(t, retirement.GlobalLockPath()) {
		t.Fatal("a refused command must not leave the global lock held")
	}

	// `intent` is not closed: the retirement has stopped nothing yet.
	if err := retirement.WriteStatus(retirement.PhaseIntent, retirement.VariantServerOnly, time.Now()); err != nil {
		t.Fatal(err)
	}

	acc, err = openIdentityAccess(t.Context(), dir, identityIntent{wait: time.Second})
	if err != nil {
		t.Fatalf("intent admits, got %v", err)
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataThatContradictsItselfRefusesRatherThanDowngrading(t *testing.T) {
	useRetirementRoot(t)

	dir := filepath.Join(t.TempDir(), "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	// A global lock file beside no record: an established exclusion whose
	// record is gone is damage, never the legacy path.
	hold, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	var damaged retirement.DamagedError
	if _, err := openIdentityAccess(t.Context(), dir, identityIntent{}); !errors.As(err, &damaged) {
		t.Fatalf("a lock without a record must refuse as damage, got %v", err)
	}

	if locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("a refused command holds nothing")
	}

	// The server's own path on such a host refuses the same way: it is not the
	// unprepared host the exemption is for.
	if _, err := serverIdentityAccess(t.Context(), dir); !errors.As(err, &damaged) {
		t.Fatalf("the server's access must refuse damage too, got %v", err)
	}
}

func TestTheUnprivilegedServerOnAnUnpreparedHostHoldsOnlyTheInitLockWhereItCouldRecreate(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the exemption is the unprivileged server's")
	}

	useRetirementRoot(t)

	parent := t.TempDir()
	dir := filepath.Join(parent, "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	acc, err := serverIdentityAccess(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	// The parent is this test's, so the server COULD recreate the directory
	// there: the init lock beside it is held, and nothing else is.
	if !locked(t, retirement.InitLockPath(dir)) {
		t.Fatal("a server whose parent it can write holds the init lock while it reads")
	}

	if locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("the unprepared host's server takes no inner lock, as before")
	}

	if acc.Lock() != nil {
		t.Fatal("the exemption carries no inner lock")
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}

	if locked(t, retirement.InitLockPath(dir)) {
		t.Fatal("release drops the init lock")
	}

	// Where an installer prepared the host between the classification and the
	// acquisition, the server proceeds on the prepared path: global, then inner.
	acct := retirement.ServiceAccount{User: "billet", UID: os.Getuid(), Group: "billet", GID: os.Getgid()}
	if err := retirement.WriteServiceAccount(acct); err != nil {
		t.Fatal(err)
	}

	hold, err := retirement.Acquire(t.Context(), retirement.AcquireOptions{Privileged: true})
	if err != nil {
		t.Fatal(err)
	}

	if err := hold.Release(); err != nil {
		t.Fatal(err)
	}

	acc, err = serverIdentityAccess(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}

	if acc.Lock() == nil || !locked(t, retirement.GlobalLockPath()) {
		t.Fatal("on a prepared host the server holds the global lock and the inner lock")
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestADarwinHostKeepsTheInnerLockAsItsWholeExclusion(t *testing.T) {
	root := useRetirementRoot(t)
	hostOS, retirement.Platform = "darwin", "darwin"

	dir := filepath.Join(t.TempDir(), "server")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	acc, err := openIdentityAccess(t.Context(), dir, identityIntent{})
	if err != nil {
		t.Fatal(err)
	}

	if !locked(t, wirecert.AuthorityLockPath(dir)) {
		t.Fatal("darwin takes the inner lock")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 0 {
		t.Fatalf("darwin writes nothing under the state root, found %v", entries)
	}

	if err := acc.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPrepareRefusesTheCallersItIsNotFor(t *testing.T) {
	old := hostOS
	t.Cleanup(func() { hostOS = old })

	hostOS = "darwin"

	if err := cmdLocalPrepare(t.Context(), []string{"--json"}); err == nil ||
		!strings.Contains(err.Error(), "Linux") {
		t.Fatalf("darwin must refuse naming the platform, got %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("the root refusal is for a caller that is not root")
	}

	hostOS = "linux"

	if err := cmdLocalPrepare(t.Context(), []string{"--json"}); err == nil ||
		!strings.Contains(err.Error(), "only root") {
		t.Fatalf("a non-root caller must be refused, got %v", err)
	}
}

func TestTheArtefactSetsAreTheProducersOwnNames(t *testing.T) {
	dir := "/var/lib/billet/server"

	names := func(set artefactSet) map[string]bool {
		targets, err := artefactTargets(dir, set)
		if err != nil {
			t.Fatal(err)
		}

		out := map[string]bool{}
		for _, target := range targets {
			out[target.Name] = target.Dir
		}

		return out
	}

	identity := names(identityArtefacts)
	for _, want := range []string{"deployment-id", "authority-created", "ca.lock", "ca", "ca/ca.crt", "ca/ca.key", "ca/ca-previous.crt", "ca/ca-previous.key"} {
		if _, ok := identity[want]; !ok {
			t.Errorf("the identity set must name %s; got %v", want, identity)
		}
	}

	if !identity["ca"] {
		t.Error("ca/ is the one directory in the identity set")
	}

	ledger := names(ledgerArtefacts)
	for _, want := range []string{"billet.db", "billet.db-wal", "billet.db-shm", "billet.lock"} {
		if _, ok := ledger[want]; !ok {
			t.Errorf("the ledger set must name %s; got %v", want, ledger)
		}
	}

	for name := range ledger {
		if strings.HasPrefix(name, "/") {
			t.Errorf("%s must be relative to the directory", name)
		}
	}
}
