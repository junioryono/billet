package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// identityAccess is what a command holds from before its first access to an
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
type identityAccess struct {
	dir       string
	exclusion wirecert.Exclusion
	lock      *wirecert.AuthorityLock
	init      *retirement.InitHold
	host      *hostLock
	account   *retirement.ServiceAccount
	created   bool
}

// identityIntent says whether the caller may CREATE the directory when it is
// positively absent, how long it waits for a held lock, and whether it already
// holds the lifecycle lock (a restore or a recovery, which take it before their
// first identity access), so a fresh initialisation borrows that hold rather
// than refusing itself as "another lifecycle command".
type identityIntent struct {
	create        bool
	wait          time.Duration
	lifecycleHeld bool
}

// identityAccessWait is the bound an operator command waits for a held lock
// before it says who holds it.
const identityAccessWait = 30 * time.Second

// openIdentityAccess resolves the host's mode for dir and takes what it
// requires, in the one lock order: the initialisation lock beside the directory
// (a fresh host only), the lifecycle lock (a root initialiser only), the global
// lock (a prepared host), the inner lock.
func openIdentityAccess(ctx context.Context, dir string, intent identityIntent) (*identityAccess, error) {
	if dir == "" {
		return nil, errors.New("billet: an identity directory is needed and the configuration names none")
	}

	acc := &identityAccess{dir: dir}

	if !retirement.SupportedHere() {
		// A platform without the global exclusion: the inner lock alone, the
		// directory created when the caller may create, as before.
		if err := acc.lockInner(ctx, wirecert.Exclusion{Legacy: true, Create: intent.create, Wait: intent.wait}); err != nil {
			return nil, err
		}

		return acc, nil
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

		return acc, nil
	case retirement.ModePrepared:
		acct := class.Account
		acc.account = &acct
	}

	ex, err := wirecert.ResolveExclusion(ctx, dir, intent.wait)
	if err != nil {
		return nil, err
	}

	acc.exclusion = ex

	if err := acc.lockInner(ctx, ex); err != nil {
		return nil, errors.Join(err, acc.exclusion.Release())
	}

	return acc, nil
}

// initialise is the fresh-initialisation branch: the lock beside the directory,
// the lifecycle lock when root, all four absences re-established under them,
// the directory created, the inner lock taken inside it.
func (a *identityAccess) initialise(ctx context.Context, intent identityIntent) error {
	if !intent.create {
		return fmt.Errorf("billet: %s does not exist, and this command creates nothing; run `billet check` "+
			"or `billet local up` to initialise this host first", a.dir)
	}

	wait := intent.wait
	if wait <= 0 {
		wait = identityAccessWait
	}

	initCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	init, err := retirement.AcquireInit(initCtx, a.dir, nil, 0)
	if err != nil {
		return err
	}

	a.init = init

	if os.Geteuid() == 0 && !intent.lifecycleHeld {
		lock, err := lifecycleLock()
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
		ex, err := wirecert.ResolveExclusion(ctx, a.dir, intent.wait)
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

	if err := a.lockInner(ctx, wirecert.Exclusion{Legacy: true, Wait: intent.wait}); err != nil {
		return errors.Join(err, a.Release())
	}

	return nil
}

func (a *identityAccess) lockInner(ctx context.Context, ex wirecert.Exclusion) error {
	lock, err := wirecert.LockAuthorityWith(ctx, a.dir, ex)
	if err != nil {
		return err
	}

	a.lock = lock

	return nil
}

// Lock is the inner lock, for the wirecert entry points that require it held.
func (a *identityAccess) Lock() *wirecert.AuthorityLock { return a.lock }

// Account is the recorded service account on a prepared host, nil otherwise.
func (a *identityAccess) Account() *retirement.ServiceAccount { return a.account }

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
func (a *identityAccess) Release() error {
	if a == nil {
		return nil
	}

	var errs []error

	if err := a.handBack(); err != nil {
		errs = append(errs, err)
	}

	if a.lock != nil {
		errs = append(errs, a.lock.Release())
		a.lock = nil
	}

	errs = append(errs, a.exclusion.Release())

	if a.host != nil {
		errs = append(errs, a.host.release())
		a.host = nil
	}

	if a.init != nil {
		errs = append(errs, a.init.Release())
		a.init = nil
	}

	return errors.Join(errs...)
}

// handBack gives the identity artefacts to the recorded service account when
// this process is root, holds the inner lock, and the host records one now.
// A record that cannot be read is a hand-back that cannot be made, reported;
// no record is nothing to give things to.
func (a *identityAccess) handBack() error {
	if os.Geteuid() != 0 || a.lock == nil || !retirement.SupportedHere() {
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

	return handBackIdentity(a.dir, acct, identityArtefacts)
}

// serverIdentityAccess is the control plane's own take on the exclusion, with
// the one exemption the protocol keeps: an UNPRIVILEGED server on a host no
// installer has prepared (no record, positively no global lock) starts as it
// always did, without the inner lock, because it cannot open a root-created
// one and no retirement can run on such a host. Where its identity directory's
// parent is writable by it, it still takes the initialisation lock beside the
// directory while it reads, so an installer's publication cannot cross its first
// identity access; that lock is released with the access.
//
// A nil access is the exemption; every method of *identityAccess tolerates it.
func serverIdentityAccess(ctx context.Context, dir string) (*identityAccess, error) {
	if os.Geteuid() == 0 || !retirement.SupportedHere() {
		return openIdentityAccess(ctx, dir, identityIntent{create: true, wait: identityAccessWait})
	}

	class, err := retirement.Classify(dir)
	if err != nil {
		return nil, err
	}

	if class.Mode == retirement.ModePrepared {
		return openIdentityAccess(ctx, dir, identityIntent{create: true, wait: identityAccessWait})
	}

	acc := &identityAccess{dir: dir}

	if parentWritable(dir) {
		initCtx, cancel := context.WithTimeout(ctx, identityAccessWait)
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
			full, err := openIdentityAccess(ctx, dir, identityIntent{create: true, wait: identityAccessWait})
			if err != nil {
				return nil, errors.Join(err, acc.Release())
			}

			full.init, acc.init = acc.init, nil

			return full, nil
		}
	}

	return acc, nil
}

// parentWritable reports whether this process may create entries in the
// identity directory's parent, which is exactly where an unprivileged server
// could recreate a moved directory.
func parentWritable(dir string) bool {
	return unix.Access(filepath.Dir(dir), unix.W_OK) == nil
}

// handBackLedger gives the service account the ledger files a privileged open
// attempt created (or would have), on a prepared host, as root; anything else
// is a no-op. It runs after the attempt whether or not the open succeeded,
// because the state opener creates the lock before it connects.
func handBackLedger(dir string) error {
	if os.Geteuid() != 0 || !retirement.SupportedHere() {
		return nil
	}

	class, err := retirement.Classify(dir)
	if err != nil || class.Mode != retirement.ModePrepared {
		// A damaged host refuses elsewhere; the hand-back is not where that is
		// judged, and a legacy host has nothing to give things to.
		return nil //nolint:nilerr // the classification's refusal belongs to the open, not to the hand-back
	}

	return handBackIdentity(dir, class.Account, ledgerArtefacts)
}

// artefactSet names what a privileged command can create inside an identity
// directory, from the producers' own path helpers, never spelled twice.
type artefactSet int

const (
	// identityArtefacts: what the identity and authority code creates.
	identityArtefacts artefactSet = iota
	// ledgerArtefacts: what a ledger open creates.
	ledgerArtefacts
)

// handBackIdentity gives the service account the artefacts a privileged command
// created inside dir, through lifeops' descriptor-based repair: only root-owned
// regular files with one link on the directory's filesystem, the two
// directories excepted, nothing walked. It is the one re-owning mechanism;
// `local up`'s preflight repair is the same function over the ledger set.
func handBackIdentity(dir string, acct retirement.ServiceAccount, set artefactSet) error {
	targets, err := artefactTargets(dir, set)
	if err != nil {
		return err
	}

	_, err = converge().RepairPaths(dir, targets, acct.UID, acct.GID)

	return err
}

// artefactTargets names the set relative to dir AS THE REPAIR WANTS IT: through
// filepath.Rel against the cleaned directory, because the producers' helpers
// clean their paths and a directory spelled with a trailing slash would
// otherwise leave every target absolute, which the repair's root refuses. A
// name that escapes the directory is a helper that changed and is refused.
func artefactTargets(dir string, set artefactSet) ([]lifeops.RepairTarget, error) {
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
	case ledgerArtefacts:
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
