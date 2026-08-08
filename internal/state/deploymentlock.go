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

	// RESOLVED ONCE, THEN HELD — and this is the second attempt at that sentence
	// being true. MkdirAll, a stat, a chmod and an open are four independent
	// pathname resolutions, and between any two of them the directory can be
	// replaced, so the thing checked need not be the thing used. The first fix
	// used os.Root, which was only most of the way there: it still needed a
	// separate os.Lstat to ask whether the name was a symlink, and that second
	// resolution could describe a different directory than the one the handle
	// held.
	//
	// O_DIRECTORY|O_NOFOLLOW answers both at once. The name is walked a single
	// time, a symlinked final component is refused by the kernel rather than
	// diagnosed afterwards, and the descriptor keeps referring to that inode
	// however the name is rearranged later.
	//
	// What it does NOT cover is an untrusted-writable ANCESTOR, because MkdirAll
	// above still walks the path by name. That is a real residual and it is
	// recorded rather than implied away.
	dirf, err := openLockDir(dir)
	if err != nil {
		return unplaceable(opts, fmt.Sprintf("cannot open %s (%v)", dir, err))
	}

	// Closed once the lock file is open: the flock lives on the FILE's
	// descriptor, so the directory handle has no further job.
	defer func() { _ = dirf.Close() }()

	shared, gid, err := checkLockDir(dirf, dir, operatorChose)
	if err != nil {
		return unplaceable(opts, err.Error())
	}

	name := "deployment-" + id + ".lock"
	path := filepath.Join(dir, name)

	lock, err := lockFileIn(dirf, name, path, shared, gid)
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
//
// A SHARED DIRECTORY MUST BE SETGID, and that requirement is not bureaucracy —
// it is the only thing that makes the sharing work. Without it a new file takes
// the CREATOR'S primary group, so a service account whose primary group is
// `service` and whose supplemental group is `billet` produces a lock file owned
// by `service`: mode 0660, every permission bit the checks ask for, and still
// unopenable by the operator it was widened for. Group-writable proves that
// somebody intended sharing; setgid is what determines WHO. So the directory's
// gid is returned and the lock file is required to match it.
func checkLockDir(dirf *os.File, dir string, operatorChose bool) (bool, fileGroup, error) {
	// ONE observation, of the descriptor. There is no second Lstat asking whether
	// the name is a symlink, because O_DIRECTORY|O_NOFOLLOW already refused that
	// at open time — and a separate resolution could have described a different
	// directory than this handle refers to, which is what made the previous
	// version's "resolved once" claim only nearly true.
	info, err := dirf.Stat()
	if err != nil {
		return false, fileGroup{}, fmt.Errorf("cannot inspect %s (%w)", dir, err)
	}

	if !info.IsDir() {
		return false, fileGroup{}, fmt.Errorf("%s is not a directory", dir)
	}

	perm := info.Mode().Perm()

	if perm&0o002 != 0 {
		return false, fileGroup{}, fmt.Errorf(
			"%s is world-writable (%o), so any local user could hold the lock file and keep "+
				"billet from starting", dir, perm)
	}

	setgid := info.Mode()&os.ModeSetgid != 0

	if !operatorChose {
		if perm&0o020 == 0 {
			return false, fileGroup{}, nil
		}

		// SETGID MEANS SOMEONE SHARED THIS ON PURPOSE, so tightening it would be
		// billet quietly dismantling an arrangement it did not make — stripping the
		// bit and locking out the other account, on no evidence beyond this
		// invocation happening to omit lock_dir. Refuse and say what to do.
		if setgid {
			return false, fileGroup{}, fmt.Errorf(
				"%s is a setgid group-shared directory (%o) but this billet did not ask for a "+
					"shared one; set server.lock_dir to it explicitly if the sharing is intended, "+
					"or point server.lock_dir somewhere private", dir, info.Mode().Perm())
		}

		// Tightened rather than refused: billet picked this path, nobody declared
		// it shared, and state.Open sets the precedent one screen up. Through the
		// descriptor, so it cannot land on something swapped in since the stat.
		if err := dirf.Chmod(0o700); err != nil {
			return false, fileGroup{}, fmt.Errorf("cannot tighten %s from %o (%w)", dir, perm, err)
		}

		return false, fileGroup{}, nil
	}

	if perm&0o020 == 0 {
		return false, fileGroup{}, nil
	}

	// BOTH WAYS OUT ARE NAMED, because only one of them was and it was the wrong
	// one for the commonest case. A single-user operator who pointed lock_dir at
	// their own 0770 directory is not sharing with anybody; telling them only to
	// `chmod g+s` sends them to set up a cross-account arrangement they do not
	// want, when dropping group write is the answer.
	if !setgid {
		return false, fileGroup{}, fmt.Errorf(
			"%s is group-writable (%o) but not setgid, so a lock file created there takes its "+
				"creator's primary group rather than the directory's: an account sharing this "+
				"directory through a supplemental group would produce a lock the other account "+
				"still cannot open. If two accounts really do share this directory, run "+
				"`chmod g+s %s`; if it is only this one, run `chmod g-w %s`", dir, perm, dir, dir)
	}

	gid, err := dirGID(info)
	if err != nil {
		return false, fileGroup{}, fmt.Errorf("cannot read the group of %s (%w)", dir, err)
	}

	return true, gid, nil
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
