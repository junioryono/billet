package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// LockDeployment takes the host-wide lock for a deployment identity.
//
// A lock that could not be PLACED is not an error: it returns a lock whose
// Degraded() reports why. That is a deliberate downgrade rather than an
// oversight — a host with no usable cache directory is a legitimate single
// deployment far more often than it is two, and refusing to boot there would
// trade a hazard nobody has hit for an outage everybody would. Behaviour falls
// back to what it was before this existed: the directory lock alone. The caller
// is expected to say so out loud.
//
// A CONTENDED lock IS an error, because that one is not ambiguous: something
// else is running under this identity right now.
func LockDeployment(id string) (*DeploymentLock, error) {
	if id == "" {
		return nil, errors.New("state: a deployment lock needs an identity")
	}

	// CHECKED AGAIN HERE, not only where the identity is read. This function
	// builds a filename out of the value, and the failure mode of not checking is
	// the bad one: an id containing a path separator lands outside the lock
	// directory or fails to open, and a failure to open DEGRADES — so the
	// protection would switch itself off and report a cache-directory problem.
	// The check that matters is the one next to the interpolation.
	if err := validDeploymentID(id); err != nil {
		return nil, fmt.Errorf("state: refusing to lock: %w", err)
	}

	dir, err := deploymentLockDir()
	if err != nil {
		return &DeploymentLock{degraded: fmt.Sprintf("no cache directory (%v)", err)}, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return &DeploymentLock{degraded: fmt.Sprintf("cannot create %s (%v)", dir, err)}, nil
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
		return &DeploymentLock{degraded: fmt.Sprintf("cannot lock %s (%v)", path, err)}, nil
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

// deploymentLockDir picks a per-user location for host-wide locks.
//
// The user cache directory rather than /tmp, and the reason is that /tmp is
// world-writable: any local user could pre-create the lock file and hold it,
// keeping billet from ever starting. A cache directory is the user's own.
//
// THE RESIDUAL IS TWO DIFFERENT USERS sharing one container runtime. They get
// different lock directories and so do not collide, while their containers do.
// Closing that needs a location both can reach and neither can abuse, which is a
// system-integration decision (a systemd RuntimeDirectory, say) rather than
// something this package can pick correctly for every deployment.
func deploymentLockDir() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(cache, "billet", "locks"), nil
}
