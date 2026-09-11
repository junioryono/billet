package retirement

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// BootstrapRequest is what a privileged installer knows when it prepares a host.
type BootstrapRequest struct {
	// Account is the installer's VALIDATED service account, never read back from
	// the record this bootstrap is about to write.
	Account ServiceAccount
	// IdentityDir is the server's configured identity directory, which the
	// bootstrap creates on a POSITIVELY FRESH host (no global lock and no
	// status beside it) so that no metadata ever exists beside a missing
	// directory there; a directory absent beside existing metadata was moved
	// by a retirement or damaged by hand, and is left absent.
	IdentityDir string
	// InnerLock is the identity directory's own authority lock path
	// (`wirecert.AuthorityLockPath`), repaired when present; named by the caller
	// because this leaf may not import the package that owns the name.
	InnerLock string
	// Wait bounds every blocking acquisition.
	Wait time.Duration
}

// BootstrapResult is what the installer reads before it decides anything else.
type BootstrapResult struct {
	// Status is the published status at the time of the bootstrap, typed; a
	// closed one lets the record be restored and forbids the installer's later
	// starts and enables, which it judges from this.
	Status   Status
	Presence StatusPresence
	Created  bool // the identity directory was created by this bootstrap
	// IdentityDirAbsent says the configured directory is absent and this
	// bootstrap left it so, because a global lock or a status already existed
	// beside it: a retired or damaged host, never a fresh one.
	IdentityDirAbsent bool
	Repaired          []string
}

// ErrNotPrivileged is a bootstrap attempted by a caller that is not root.
var ErrNotPrivileged = errors.New("retirement: only root prepares a host")

// Bootstrap is the privileged path that moves a host onto the global exclusion,
// or repairs its metadata: `local up`, the package's postinstall and the host
// role call it, and nothing else does (an ordinary command never bootstraps to
// admit itself).
//
// THE ORDER IS THE INVARIANT. The init lock beside the directory first (so a
// fresh initialisation in flight is waited for), the identity directory created
// before any metadata AND ONLY WHEN NONE EXISTS YET (the global lock and the
// status both positively absent under the init lock, which every creator of
// either takes first: so a global lock or a status beside an absent directory
// is always a retirement's move or damage, never a fresh host, and this never
// recreates what a retirement archived), then the global lock (creating it),
// the status read (unreadable or malformed refuses: the installer cannot tell
// whether it is restoring a retired host), the record published from the
// validated account, the locks repaired by descriptor. A closed status does not
// stop the publication; it is returned, and the installer refuses whatever a
// closed authority forbids.
func Bootstrap(ctx context.Context, req BootstrapRequest) (BootstrapResult, error) {
	var res BootstrapResult

	if os.Geteuid() != 0 {
		return res, ErrNotPrivileged
	}

	if err := req.Account.validate(); err != nil {
		return res, fmt.Errorf("retirement: refuse to bootstrap with %+v: %w", req.Account, err)
	}

	if req.IdentityDir == "" {
		return res, errors.New("retirement: bootstrap without an identity directory")
	}

	wait := req.Wait
	if wait <= 0 {
		wait = time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	if err := os.MkdirAll(Root, 0o755); err != nil {
		return res, fmt.Errorf("retirement: create %s: %w", Root, err)
	}

	init, err := AcquireInit(ctx, req.IdentityDir, &req.Account, 0)
	if err != nil {
		return res, err
	}

	err = bootstrapUnderInit(ctx, req, &res)

	// The releases are joined rather than deferred and discarded: a lock that
	// failed to release is the next installer's "held by another billet".
	return res, errors.Join(err, init.Release())
}

func bootstrapUnderInit(ctx context.Context, req BootstrapRequest, res *BootstrapResult) error {
	fresh, err := metadataAbsent()
	if err != nil {
		return err
	}

	if fresh {
		created, err := ensureIdentityDir(req.IdentityDir, req.Account)
		if err != nil {
			return err
		}

		res.Created = created
	} else {
		present, err := exists(req.IdentityDir)
		if err != nil {
			return fmt.Errorf("retirement: examine %s: %w", req.IdentityDir, err)
		}

		res.IdentityDirAbsent = !present
	}

	hold, err := Acquire(ctx, AcquireOptions{Privileged: true, Account: &req.Account})
	if err != nil {
		return err
	}

	return errors.Join(bootstrapUnderHold(req, res), hold.Release())
}

// metadataAbsent reports whether the global lock and the status are both
// positively absent, the two observations that make an absent identity
// directory a fresh host's rather than a retirement's remainder. A failed
// observation is neither and refuses.
func metadataAbsent() (bool, error) {
	lock, err := exists(GlobalLockPath())
	if err != nil {
		return false, fmt.Errorf("retirement: examine %s: %w", GlobalLockPath(), err)
	}

	status, err := exists(StatusPath())
	if err != nil {
		return false, fmt.Errorf("retirement: examine %s: %w", StatusPath(), err)
	}

	return !lock && !status, nil
}

func bootstrapUnderHold(req BootstrapRequest, res *BootstrapResult) error {
	st, presence, statusErr := ReadStatus()
	res.Status, res.Presence = st, presence

	if err := admitBootstrapStatus(presence, statusErr); err != nil {
		return err
	}

	if err := WriteServiceAccount(req.Account); err != nil {
		return err
	}

	repaired, err := RepairLocks(req.Account, req.InnerLock)
	res.Repaired = repaired

	return err
}

// admitBootstrapStatus is the one rule for what a bootstrap publishes over:
// an absent or a present status (a closed one is returned, not refused), and
// never one that could not be read OR parsed, because a status the installer
// cannot judge may be a retired host's, and metadata published over it would
// admit an ordinary writer into a directory a retirement is moving.
func admitBootstrapStatus(presence StatusPresence, cause error) error {
	switch presence {
	case StatusAbsent, StatusPresent:
		return nil
	default:
		return ErrStatusUnknown{Cause: cause}
	}
}

// ensureIdentityDir creates the configured identity directory owned by the
// service account at 0700 when it is positively absent, its parent synced; an
// existing directory is left as it is (its ownership is `local up`'s repair).
func ensureIdentityDir(dir string, acct ServiceAccount) (bool, error) {
	if _, err := os.Lstat(dir); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("retirement: examine %s: %w", dir, err)
	}

	if err := os.Mkdir(dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}

		return false, fmt.Errorf("retirement: create %s: %w", dir, err)
	}

	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_DIRECTORY, 0)
	if err != nil {
		return false, fmt.Errorf("retirement: open %s: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Chown(acct.UID, acct.GID); err != nil {
		return false, fmt.Errorf("retirement: own %s: %w", dir, err)
	}

	if err := syncDir(filepath.Dir(dir)); err != nil {
		return false, err
	}

	return true, nil
}

// RepairLocks gives the two lock files the owners the recorded account needs,
// BY DESCRIPTOR AND WHATEVER THEIR PREVIOUS OWNER: the global lock root:<gid>
// 0660, the inner lock <uid>:<gid> 0600. A lock is never unlinked or recreated,
// since a holder's flock lives on the inode; a symlink at either name refuses,
// and so does a file with more than one link or one on another filesystem than
// its directory's, because giving an inode away through one name gives it away
// through every name it has. An absent inner lock is left absent (its creator
// owns it when it appears). The answer is never nil: an installer reports it as
// a list, and nothing repaired is an empty list, not an absent one.
func RepairLocks(acct ServiceAccount, innerLock string) ([]string, error) {
	if os.Geteuid() != 0 {
		return nil, ErrNotPrivileged
	}

	repaired := []string{}

	global, err := reownLock(GlobalLockPath(), 0, acct.GID, 0o660)
	if err != nil {
		return nil, err
	}

	if global {
		repaired = append(repaired, GlobalLockPath())
	}

	if innerLock != "" {
		inner, err := reownLock(innerLock, acct.UID, acct.GID, 0o600)
		if err != nil {
			return repaired, err
		}

		if inner {
			repaired = append(repaired, innerLock)
		}
	}

	return repaired, nil
}

func reownLock(path string, uid, gid int, mode os.FileMode) (bool, error) {
	f, err := openLockFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("retirement: open %s to repair its owner: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("retirement: stat %s: %w", path, err)
	}

	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("retirement: %s is not a regular file, so it is not a lock billet wrote", path)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("retirement: %s: ownership could not be read", path)
	}

	// THE CHECKS BEFORE ANY CHANGE, ON THE OPEN DESCRIPTOR: one link, because a
	// hard link to a file elsewhere passes every regular-file check and a chown
	// through this name would give that file away through its other one; and
	// the directory's own filesystem, because a lock billet wrote lives beside
	// its directory and a file from elsewhere is not it.
	if st.Nlink != 1 {
		return false, fmt.Errorf("retirement: %s has %d links, so re-owning it would give away "+
			"whatever else names that inode; billet leaves it alone", path, st.Nlink)
	}

	dirInfo, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return false, fmt.Errorf("retirement: examine %s's directory: %w", path, err)
	}

	if dirSt, ok := dirInfo.Sys().(*syscall.Stat_t); !ok || dirSt.Dev != st.Dev {
		return false, fmt.Errorf("retirement: %s is not on its directory's filesystem, so it is not "+
			"a lock billet wrote; billet leaves it alone", path)
	}

	changed := false

	if int(st.Uid) != uid || int(st.Gid) != gid {
		if err := f.Chown(uid, gid); err != nil {
			return false, fmt.Errorf("retirement: own %s: %w", path, err)
		}

		changed = true
	}

	if info.Mode().Perm() != mode {
		if err := f.Chmod(mode); err != nil {
			return false, fmt.Errorf("retirement: set the mode of %s: %w", path, err)
		}

		changed = true
	}

	return changed, nil
}
