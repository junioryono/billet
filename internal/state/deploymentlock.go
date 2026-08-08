package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrDeploymentLocked means another billet is already running under this
// deployment identity.
var ErrDeploymentLocked = errors.New("state: this deployment is already running on this host")

// DeploymentLock is an exclusive host-wide lock on one deployment identity.
//
// THE DIRECTORY LOCK GUARDS A PATH; THIS GUARDS AN IDENTITY, and the difference
// is a hole I documented rather than closed. `billet.lock` is flocked inside the
// state directory, so a COPY of that directory is a different inode and both
// copies lock happily. Both then carry the same deployment id — deliberately,
// because the copy's containers are labelled with it — so both enumerate the
// same compute against the same daemon. Recovery adopts rather than destroys,
// which makes the consequence smaller than it was, but "two processes managing
// one set of containers" is not a state either of them can reason about: each
// will heartbeat leases the other owns and hold capacity for work it did not
// start.
//
// Keyed by the identity precisely so a copy collides. That is the point: the
// copy IS the same installation as far as its containers are concerned, and the
// honest answer to running it twice is to refuse.
type DeploymentLock struct {
	lock *dirLock
	path string

	// degraded says WHY no lock was taken, when none was. Empty means the lock is
	// held.
	//
	// A reason rather than a nil lock and a nil error. The caller has to tell an
	// operator that a protection is absent, and "here is a lock that is not a
	// lock" with no explanation is exactly the shape that gets logged as a shrug.
	degraded string
}

// LockOptions configures where the host-wide lock goes and what happens when it
// cannot be placed.
type LockOptions struct {
	// Dir overrides the default location. Empty uses defaultLockDir.
	Dir string
	// AllowUnplaceable downgrades "nowhere to put the lock" from an error to a
	// degraded lock the caller must report. Contention is never downgraded.
	AllowUnplaceable bool
}

// LockDeployment takes the host-wide lock for a deployment identity.
//
// FAILING TO PLACE THE LOCK IS AN ERROR, and it did not used to be. The first
// version returned a degraded lock for every reason the file could not be
// locked, on the reasoning that a host with nowhere to put one is far more often
// a single deployment than two, so refusing to boot would trade a rare hazard
// for a common outage.
//
// That is the wrong shape even where the conclusion is defensible, because it
// DERIVES AUTHORIZATION FROM AN I/O FAILURE. A symlink loop, a permissions
// change, ENOLCK, descriptor exhaustion, or a service manager that provides no
// HOME would each silently switch the protection off, leaving one log line among
// many as the operator's only evidence. Every one of those is a misconfiguration
// that looks exactly like the benign case from in here. An operator who knows
// their host has nowhere to put a lock says so with AllowUnplaceable; billet
// does not decide it for them.
//
// A CONTENDED lock is an error under either setting, because that one is not
// ambiguous: something else is running under this identity right now.
func LockDeployment(id string, opts LockOptions) (*DeploymentLock, error) {
	if id == "" {
		return nil, errors.New("state: a deployment lock needs an identity")
	}

	// CHECKED AGAIN HERE, not only where the identity is read. This function
	// builds a filename out of the value, and the failure mode of not checking is
	// the bad one: an id containing a path separator lands outside the lock
	// directory or fails to open, and a failure to open is an unplaceable lock.
	// The check that matters is the one next to the interpolation.
	if err := validDeploymentID(id); err != nil {
		return nil, fmt.Errorf("state: refusing to lock: %w", err)
	}

	dir, operatorChose := opts.Dir, opts.Dir != ""
	if !operatorChose {
		var err error

		dir, err = defaultLockDir()
		if err != nil {
			return unplaceable(opts, fmt.Sprintf("no default lock directory (%v)", err))
		}
	}

	// ABSOLUTE, AND RESOLVED BEFORE IT IS USED OR LOGGED. A relative lock_dir is
	// interpreted against the working directory, so one config saying
	// `lock_dir: locks` puts a systemd unit started in / and an operator started
	// in /srv/billet into DIFFERENT collision domains — and both would log the
	// same relative string, so the diagnostic that exists to expose a mismatch
	// would conceal this one.
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf(
			"state: server.lock_dir must be an absolute path, got %q: a relative one resolves "+
				"against each process's working directory, so two billets sharing this config "+
				"could lock different files while reporting the same path", dir)
	}

	dir = filepath.Clean(dir)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return unplaceable(opts, fmt.Sprintf("cannot create %s (%v)", dir, err))
	}

	shared, err := checkLockDir(dir, operatorChose)
	if err != nil {
		return unplaceable(opts, err.Error())
	}

	path := filepath.Join(dir, "deployment-"+id+".lock")

	lock, err := lockFile(path, shared)
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, fmt.Errorf(
				"%w: identity %s is held by another process (%s). Two billets sharing one "+
					"identity manage the same containers and heartbeat each other's leases — "+
					"if this is a COPY of a state directory, give it its own by deleting its "+
					"deployment-id file",
				ErrDeploymentLocked, id, path)
		}

		// Anything else — a read-only filesystem, a permissions problem — is the
		// same "cannot lock here" situation as a missing directory.
		return unplaceable(opts, fmt.Sprintf("cannot lock %s (%v)", path, err))
	}

	return &DeploymentLock{lock: lock, path: path}, nil
}

// Release drops the lock. Safe on a degraded lock, which holds nothing.
func (d *DeploymentLock) Release() error {
	if d == nil || d.lock == nil {
		return nil
	}

	return d.lock.release()
}

// Path reports where the lock lives, for diagnostics. Empty when degraded.
func (d *DeploymentLock) Path() string {
	if d == nil {
		return ""
	}

	return d.path
}

// Degraded reports why no lock was taken, or "" when one was.
func (d *DeploymentLock) Degraded() string {
	if d == nil {
		return "no lock was requested"
	}

	return d.degraded
}

// checkLockDir refuses a directory an untrusted party could interfere with, and
// reports whether it is a SHARED one.
//
// MkdirAll IS NOT ENOUGH, which state.Open already knew and this did not: its
// mode applies only to components it CREATES, so an existing directory keeps
// whatever permissions it had. That matters more here than it looks, because
// failing closed made it matter: with degradation gone, a directory an
// unprivileged user can write is no longer just a way to defeat the lock by
// unlinking the file — it is a way to hold the filename and keep billet from
// ever starting. That is precisely the denial of service that ruled out /tmp in
// the first place, so the two fixes interact and this is where it is paid for.
//
// TWO REGIMES, because the answer genuinely differs:
//
//   - A path BILLET chose (the per-user default) must be ours alone. Nobody else
//     has any business writing there, so group- and world-writable are both
//     refused, and the mode is tightened the way state.Open tightens its own.
//   - A path the OPERATOR chose is allowed to be group-writable, because that is
//     the entire point of server.lock_dir: a setgid directory shared by a service
//     account and an operator who both reach the same docker socket. They are
//     trusted with each other's compute by construction. World-writable is still
//     refused under both.
func checkLockDir(dir string, operatorChose bool) (bool, error) {
	// Lstat, not Stat: a symlink here is someone redirecting the lock, and
	// following it to ask about the target would answer the wrong question.
	info, err := os.Lstat(dir)
	if err != nil {
		return false, fmt.Errorf("cannot inspect %s (%w)", dir, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%s is a symlink, so what it locks can be redirected", dir)
	}

	if !info.IsDir() {
		return false, fmt.Errorf("%s is not a directory", dir)
	}

	perm := info.Mode().Perm()

	if perm&0o002 != 0 {
		return false, fmt.Errorf(
			"%s is world-writable (%o), so any local user could hold the lock file and keep "+
				"billet from starting", dir, perm)
	}

	if !operatorChose {
		if perm&0o020 != 0 {
			// Tightened rather than refused: billet picked this path, so it is
			// billet's to correct — and state.Open sets the precedent one screen up.
			if err := os.Chmod(dir, 0o700); err != nil {
				return false, fmt.Errorf("cannot tighten %s from %o (%w)", dir, perm, err)
			}
		}

		return false, nil
	}

	return perm&0o020 != 0, nil
}

// unplaceable is what happens when there is nowhere to put the lock: an error
// unless the operator has said otherwise.
func unplaceable(opts LockOptions, why string) (*DeploymentLock, error) {
	if !opts.AllowUnplaceable {
		return nil, fmt.Errorf(
			"state: cannot place the host-wide deployment lock: %s. Without it, two billets "+
				"carrying one identity would manage the same containers. Set server.lock_dir to "+
				"a directory this process can write and every billet sharing this container "+
				"runtime can reach, or set server.allow_unlocked_deployment to start without "+
				"the protection", why)
	}

	return &DeploymentLock{degraded: why}, nil
}

// defaultLockDir picks a per-user location for host-wide locks.
//
// NOT THE CACHE DIRECTORY, which is where this started. A cache directory's
// whole contract is that its contents may be deleted at any time by anyone —
// cleaners, packagers, the user reclaiming disk. Deleting the lock file while it
// is held does not release the flock, but it does unlink the PATH from the
// locked inode: the next process creates a new file there, locks that, and both
// run. So the one property a lock file needs is exactly the one a cache
// directory refuses to provide. The state directory instead ($XDG_STATE_HOME on
// Linux, Application Support on darwin), whose contract is persistence.
//
// This is per-user, and that is a DEFAULT rather than an answer. Two different
// users sharing one container runtime get different directories and so do not
// collide, while their containers do; so do two containers that share a docker
// socket but have private filesystems. Both need server.lock_dir pointed
// somewhere they all meet, which is a system-integration decision this package
// cannot make correctly for every deployment. What it can do is refuse to
// pretend: the path is reported so a mismatch is visible rather than silent.
//
// Not /tmp, under any circumstances: it is world-writable, so any local user
// could pre-create the lock file and hold it, keeping billet from ever starting.
func defaultLockDir() (string, error) {
	home, err := os.UserHomeDir()

	// GOOS FIRST, and the previous order was a documentation bug as much as a
	// code one: the comment said darwin uses Application Support while the code
	// consulted XDG_STATE_HOME on every platform, so setting that variable moved
	// a macOS lock somewhere the docs said it could not go.
	if runtime.GOOS == "darwin" {
		if err != nil {
			return "", err
		}

		return filepath.Join(home, "Library", "Application Support", "billet", "locks"), nil
	}

	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "billet", "locks"), nil
	}

	if err != nil {
		return "", err
	}

	return filepath.Join(home, ".local", "state", "billet", "locks"), nil
}
