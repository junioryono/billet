// Package hostauthority is what a command on this host holds before it touches
// an identity directory: the exclusion the host's metadata requires (the global
// lock on a prepared host, the inner lock otherwise), the lock beside a
// directory a fresh initialisation creates, the lifecycle lock that keeps two
// lifecycle commands from interleaving, and the hand-back of what a privileged
// command created to the service account. It was cmd/billet's until the
// commands moved out of it (#356 Phase 4); every command, and the operator
// ledger opens in internal/app, take it through here.
package hostauthority

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// Access is what a command holds from before its first access to an
// identity directory until it is done with it: the exclusion the host's
// metadata requires (the global lock, admitted, on a prepared host; the legacy
// inner-lock path with its recheck otherwise), the inner lock, and, for a fresh
// initialisation, the lock beside the directory it is creating.
//
// EVERY CREATING OR REPAIRING ENTRYPOINT TAKES ONE BEFORE `state.DeploymentID`,
// `wirecert.LoadOrCreateCA` OR THE LEDGER OPEN, because each of those creates
// on first use and a retirement renames the directory they would create into.
// The one exemption is the unprivileged server's startup on a host no installer
// has prepared, where no retirement can run.
type Access struct {
	dir       string
	exclusion wirecert.Exclusion
	lock      *wirecert.AuthorityLock
	init      *retirement.InitHold
	host      *LifecycleLock
	account   *retirement.ServiceAccount
	created   bool
	// dirMoved says this command renamed the directory under its own hold (a
	// retirement's archive), so the release hands nothing back.
	dirMoved bool
}

// Intent says whether the caller may CREATE the directory when it is
// positively absent, how long it waits for a held lock, and whether it already
// holds the lifecycle lock (a restore or a recovery, which take it before their
// first identity access), so a fresh initialisation borrows that hold rather
// than refusing itself as "another lifecycle command".
type Intent struct {
	Create        bool
	Wait          time.Duration
	LifecycleHeld bool
}

// Wait is the bound an operator command waits for a held lock before it says
// who holds it. A variable so a test can shorten the wait it proves; production
// reads the constant value.
var Wait = 30 * time.Second

// heldAccesses is every identity access this process holds, by cleaned
// directory, so a ledger factory reached by a command that already holds the
// exclusion borrows it rather than taking a second one (which one process is
// denied), and a factory reached by a command that holds none takes its own.
var heldAccesses sync.Map

func accessKey(dir string) string { return filepath.Clean(dir) }

// Held reports whether this process holds an identity access
// for dir.
func Held(dir string) bool {
	_, held := heldAccesses.Load(accessKey(dir))

	return held
}

// Under runs fn with the identity exclusion for dir held:
// borrowed when the command already holds it, taken (and released after fn)
// when it does not. The take waits the operator bound and, on Linux, creates
// nothing, so an absent directory refuses whatever the host's mode; off Linux
// there is no retirement to have moved it and the opener creates as it always
// did.
func Under(ctx context.Context, dir string, fn func() error) error {
	if Held(dir) {
		return fn()
	}

	acc, err := Open(ctx, dir,
		Intent{Create: !retirement.SupportedHere(), Wait: Wait})
	if err != nil {
		return err
	}

	return errors.Join(fn(), acc.Release())
}

// Open resolves the host's mode for dir and takes what it requires, in the one
// lock order: the initialisation lock beside the directory (a fresh host only),
// the lifecycle lock (a root initialiser only), the global lock (a prepared
// host), the inner lock. A host whose authority a retirement has closed
// refuses with retirement.ErrRetiring, which exits retirement.ExitRetiring.
func Open(ctx context.Context, dir string, intent Intent) (*Access, error) {
	if dir == "" {
		return nil, errors.New("billet: an identity directory is needed and the configuration names none")
	}

	acc := &Access{dir: dir}

	if !retirement.SupportedHere() {
		// A platform without the global exclusion: the inner lock alone, the
		// directory created when the caller may create, as before.
		if err := acc.lockInner(ctx, wirecert.Exclusion{Legacy: true, Create: intent.Create, Wait: intent.Wait}); err != nil {
			return nil, err
		}

		return acc.registered(), nil
	}

	class, err := retirement.Classify(dir)
	if err != nil {
		return nil, err
	}

	switch class.Mode {
	case retirement.ModeFresh:
		if err := acc.initialise(ctx, intent); err != nil {
			return nil, err
		}

		return acc.registered(), nil
	case retirement.ModePrepared:
		acct := class.Account
		acc.account = &acct
	}

	ex, err := wirecert.ResolveExclusion(ctx, dir, intent.Wait)
	if err != nil {
		return nil, err
	}

	acc.exclusion = ex

	if err := acc.lockInner(ctx, ex); err != nil {
		return nil, errors.Join(err, acc.exclusion.Release())
	}

	return acc.registered(), nil
}

// registered records this access as held for its directory.
func (a *Access) registered() *Access {
	if a.lock != nil {
		heldAccesses.Store(accessKey(a.dir), a)
	}

	return a
}

// initialise is the fresh-initialisation branch: the lock beside the directory,
// the lifecycle lock when root, all four absences re-established under them,
// the directory created, the inner lock taken inside it.
func (a *Access) initialise(ctx context.Context, intent Intent) error {
	if !intent.Create {
		return fmt.Errorf("billet: %s does not exist, and this command creates nothing; run `billet check` "+
			"or `billet local up` to initialise this host first", a.dir)
	}

	wait := intent.Wait
	if wait <= 0 {
		wait = Wait
	}

	initCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	init, err := retirement.AcquireInit(initCtx, a.dir, nil, 0)
	if err != nil {
		return err
	}

	a.init = init

	if os.Geteuid() == 0 && !intent.LifecycleHeld {
		lock, err := takeLifecycleLock()
		if err != nil {
			return errors.Join(err, a.Release())
		}

		a.host = lock
	}

	class, err := retirement.Classify(a.dir)
	if err != nil {
		return errors.Join(err, a.Release())
	}

	if class.Mode != retirement.ModeFresh {
		// An installer or another initialisation got there first; this caller
		// proceeds on whatever the host now is, holding its init lock still.
		ex, err := wirecert.ResolveExclusion(ctx, a.dir, intent.Wait)
		if err != nil {
			return errors.Join(err, a.Release())
		}

		a.exclusion = ex

		if class.Mode == retirement.ModePrepared {
			acct := class.Account
			a.account = &acct
		}

		if err := a.lockInner(ctx, ex); err != nil {
			return errors.Join(err, a.Release())
		}

		return nil
	}

	if err := os.Mkdir(a.dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.Join(fmt.Errorf("billet: create %s: %w", a.dir, err), a.Release())
	}

	a.created = true

	if err := a.lockInner(ctx, wirecert.Exclusion{Legacy: true, Wait: intent.Wait}); err != nil {
		return errors.Join(err, a.Release())
	}

	return nil
}

func (a *Access) lockInner(ctx context.Context, ex wirecert.Exclusion) error {
	lock, err := wirecert.LockAuthorityWith(ctx, a.dir, ex)
	if err != nil {
		return err
	}

	a.lock = lock

	return nil
}

// Moved says the directory this access guards was renamed by the command that
// holds it, so there is nothing at its name to hand back: the artefacts went
// with the directory, and a repair rooted at a name that no longer exists
// would report a failure about a move this command made on purpose.
func (a *Access) Moved() {
	if a == nil {
		return
	}

	a.dirMoved = true
}

// WasMoved reports whether Moved was called.
func (a *Access) WasMoved() bool { return a != nil && a.dirMoved }

// Dir is the identity directory this access guards.
func (a *Access) Dir() string { return a.dir }

// Lock is the inner lock, for the wirecert entry points that require it held.
func (a *Access) Lock() *wirecert.AuthorityLock { return a.lock }

// Adopt takes the inner lock under an exclusion the caller resolved itself (a
// retirement's, which resolves the global lock in its own steps) and records
// the access as this process's for dir. The exclusion is the access's from
// here: Release releases it. On an error the exclusion is still the caller's.
func Adopt(ctx context.Context, dir string, ex wirecert.Exclusion) (*Access, error) {
	acc := &Access{dir: dir, exclusion: ex, account: ex.Account}

	if err := acc.lockInner(ctx, ex); err != nil {
		return nil, err
	}

	return acc.registered(), nil
}

// HandBack gives the identity and ledger artefacts to the recorded service
// account, as Release would, without releasing anything.
func (a *Access) HandBack() error { return a.handBack() }

// ReleaseWithoutHandBack drops every lock this access holds and gives nothing
// back: for a caller that has already handed back, or must not.
func (a *Access) ReleaseWithoutHandBack() error {
	if a == nil {
		return nil
	}

	lock := a.lock
	a.lock = nil

	heldAccesses.CompareAndDelete(accessKey(a.dir), a)

	return errors.Join(lock.Release(), a.Release())
}

// Account is the recorded service account on a prepared host, nil otherwise.
func (a *Access) Account() *retirement.ServiceAccount { return a.account }

// Release hands the identity artefacts back to the service account when root
// created them on a prepared host, then drops every lock in the reverse of
// the order they were taken. Errors are joined, never dropped: a lock that did
// not release is the next command's "held by another billet".
//
// THE ACCOUNT IS READ HERE, UNDER THE LOCKS STILL HELD, and not remembered from
// the classification that opened the access: an installer may publish the
// record between that classification and this release (a legacy root writer
// holds the inner lock while `Bootstrap` publishes under the global one), and
// what such a writer created is exactly what the newly recorded account must
// be given, or the first server start after the preparation meets root-owned
// CA files it cannot read.
func (a *Access) Release() error {
	if a == nil {
		return nil
	}

	var errs []error

	if err := a.handBack(); err != nil {
		errs = append(errs, err)
	}

	if a.lock != nil {
		heldAccesses.CompareAndDelete(accessKey(a.dir), a)
		errs = append(errs, a.lock.Release())
		a.lock = nil
	}

	errs = append(errs, a.exclusion.Release())

	if a.host != nil {
		errs = append(errs, a.host.Release())
		a.host = nil
	}

	if a.init != nil {
		errs = append(errs, a.init.Release())
		a.init = nil
	}

	return errors.Join(errs...)
}

// handBack gives the identity artefacts AND the ledger artefacts to the
// recorded service account when this process is root, holds the inner lock,
// and the host records one now. The ledger set is included because a ledger
// open made under this access before an installer published the record found
// no account to hand its files to at the time (the open attempt's hand-back
// runs at once, from the record as it then stood), and a record published
// while the command held the inner lock must reach those files too. A record
// that cannot be read is a hand-back that cannot be made, reported; no record
// is nothing to give things to.
func (a *Access) handBack() error {
	if os.Geteuid() != 0 || a.lock == nil || !retirement.SupportedHere() || a.dirMoved {
		return nil
	}

	acct, err := retirement.ReadServiceAccount()

	switch {
	case errors.Is(err, retirement.ErrNoServiceAccount):
		return nil
	case err != nil:
		return fmt.Errorf("billet: the identity artefacts under %s could not be handed back to the "+
			"service account, because its record could not be read: %w", a.dir, err)
	}

	a.account = &acct

	return errors.Join(handBackSet(a.dir, acct, IdentityArtefacts), handBackSet(a.dir, acct, LedgerArtefacts))
}

// ServerAccess is the control plane's own take on the exclusion, with
// the one exemption the protocol keeps: an UNPRIVILEGED server on a host no
// installer has prepared (no record, positively no global lock) starts as it
// always did, without the inner lock, because it cannot open a root-created
// one and no retirement can run on such a host. Where its identity directory's
// parent is writable by it, it still takes the initialisation lock beside the
// directory while it reads, so an installer's publication cannot cross its first
// identity access; that lock is released with the access.
//
// A nil access is the exemption; every method of *Access tolerates it.
func ServerAccess(ctx context.Context, dir string) (*Access, error) {
	if os.Geteuid() == 0 || !retirement.SupportedHere() {
		return Open(ctx, dir, Intent{Create: true, Wait: Wait})
	}

	class, err := retirement.Classify(dir)
	if err != nil {
		return nil, err
	}

	if class.Mode == retirement.ModePrepared {
		return Open(ctx, dir, Intent{Create: true, Wait: Wait})
	}

	acc := &Access{dir: dir}

	if parentWritable(dir) {
		initCtx, cancel := context.WithTimeout(ctx, Wait)
		defer cancel()

		init, err := retirement.AcquireInit(initCtx, dir, nil, 0)
		if err != nil {
			return nil, err
		}

		acc.init = init

		// The recheck under the lock: an installer may have prepared the host
		// between the classification and the acquisition.
		again, err := retirement.Classify(dir)
		if err != nil {
			return nil, errors.Join(err, acc.Release())
		}

		if again.Mode == retirement.ModePrepared {
			full, err := Open(ctx, dir, Intent{Create: true, Wait: Wait})
			if err != nil {
				return nil, errors.Join(err, acc.Release())
			}

			full.init, acc.init = acc.init, nil

			return full, nil
		}
	}

	return acc, nil
}

// ServerWireAccess is ServerAccess in the shape the node wire takes it,
// for app.ServeNodeWire: the access around its authority read, released when
// the read is done.
func ServerWireAccess(ctx context.Context, dir string) (func() error, error) {
	acc, err := ServerAccess(ctx, dir)
	if err != nil {
		return nil, err
	}

	return acc.Release, nil
}

// parentWritable reports whether this process may create entries in the
// identity directory's parent, which is exactly where an unprivileged server
// could recreate a moved directory.
func parentWritable(dir string) bool {
	return unix.Access(filepath.Dir(dir), unix.W_OK) == nil
}

// HandBackLedger gives the service account the ledger files a privileged open
// attempt created (or would have), on a prepared host, as root; anything else
// is a no-op. It runs after the attempt whether or not the open succeeded,
// because the state opener creates the lock before it connects.
func HandBackLedger(dir string) error {
	if os.Geteuid() != 0 || !retirement.SupportedHere() {
		return nil
	}

	class, err := retirement.Classify(dir)
	if err != nil || class.Mode != retirement.ModePrepared {
		// A damaged host refuses elsewhere; the hand-back is not where that is
		// judged, and a legacy host has nothing to give things to.
		return nil //nolint:nilerr // the classification's refusal belongs to the open, not to the hand-back
	}

	return handBackSet(dir, class.Account, LedgerArtefacts)
}

// ArtefactSet names what a privileged command can create inside an identity
// directory, from the producers' own path helpers, never spelled twice.
type ArtefactSet int

const (
	// IdentityArtefacts: what the identity and authority code creates.
	IdentityArtefacts ArtefactSet = iota
	// LedgerArtefacts: what a ledger open creates.
	LedgerArtefacts
)

// repairPaths is lifeops' descriptor-based repair, a variable so a test can
// observe a hand-back without root. Only Linux hands anything back (a
// retirement's metadata exists nowhere else), so the systemd converger is the
// one that repairs.
var repairPaths = func(dir string, targets []lifeops.RepairTarget, uid, gid int) ([]string, error) {
	return lifeops.NewConverger(lifeops.NewInspector()).RepairPaths(dir, targets, uid, gid)
}

// handBackSet gives the service account the artefacts a privileged command
// created inside dir, through lifeops' descriptor-based repair: only root-owned
// regular files with one link on the directory's filesystem, the two
// directories excepted, nothing walked. It is the one re-owning mechanism;
// `local up`'s preflight repair is the same function over the ledger set.
func handBackSet(dir string, acct retirement.ServiceAccount, set ArtefactSet) error {
	targets, err := ArtefactTargets(dir, set)
	if err != nil {
		return err
	}

	_, err = repairPaths(dir, targets, acct.UID, acct.GID)

	return err
}

// ArtefactTargets names the set relative to dir AS THE REPAIR WANTS IT: through
// filepath.Rel against the cleaned directory, because the producers' helpers
// clean their paths and a directory spelled with a trailing slash would
// otherwise leave every target absolute, which the repair's root refuses. A
// name that escapes the directory is a helper that changed and is refused.
func ArtefactTargets(dir string, set ArtefactSet) ([]lifeops.RepairTarget, error) {
	base := filepath.Clean(dir)

	rel := func(path string) (string, error) {
		name, err := filepath.Rel(base, path)
		if err != nil {
			return "", fmt.Errorf("billet: %s is not under %s: %w", path, base, err)
		}

		if name == ".." || strings.HasPrefix(name, "../") || filepath.IsAbs(name) {
			return "", fmt.Errorf("billet: %s is not under %s, so it is not an identity artefact", path, base)
		}

		return name, nil
	}

	var (
		targets []lifeops.RepairTarget
		errs    []error
	)

	add := func(path string, dirTarget bool) {
		name, err := rel(path)
		if err != nil {
			errs = append(errs, err)

			return
		}

		targets = append(targets, lifeops.RepairTarget{Name: name, Dir: dirTarget})
	}

	switch set {
	case LedgerArtefacts:
		add(state.LedgerPath(dir), false)
		add(state.LedgerPath(dir)+"-wal", false)
		add(state.LedgerPath(dir)+"-shm", false)
		add(state.DirectoryLockPath(dir), false)
	default:
		add(state.DeploymentIDPath(dir), false)
		add(wirecert.AuthorityMarkerPath(dir), false)
		add(wirecert.AuthorityLockPath(dir), false)
		add(wirecert.CADir(dir), true)
		add(wirecert.CACertPath(dir), false)
		add(wirecert.CAKeyPath(dir), false)
		add(wirecert.PreviousCACertPath(dir), false)
		add(wirecert.PreviousCAKeyPath(dir), false)
	}

	return targets, errors.Join(errs...)
}
