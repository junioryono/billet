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

	dir := opts.Dir
	if dir == "" {
		var err error

		dir, err = defaultLockDir()
		if err != nil {
			return unplaceable(opts, fmt.Sprintf("no default lock directory (%v)", err))
		}
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return unplaceable(opts, fmt.Sprintf("cannot create %s (%v)", dir, err))
	}

	path := filepath.Join(dir, "deployment-"+id+".lock")

	lock, err := lockFile(path)
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
	if dir := os.Getenv("XDG_STATE_HOME"); dir != "" && filepath.IsAbs(dir) {
		return filepath.Join(dir, "billet", "locks"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "billet", "locks"), nil
	}

	return filepath.Join(home, ".local", "state", "billet", "locks"), nil
}
