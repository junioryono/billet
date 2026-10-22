package firecracker

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
)

// What a capture asks the VMM to write, inside its chroot.
//
// NOT THE NAMES A RESTORE LINKS. A restored microVM holds its snapshot as read-only
// links named snapshot.state and snapshot.mem, owned by billet; a capture of it
// asked to write those names would be refused, after the guest was already paused.
const (
	captureStateFile  = "capture.state"
	captureMemoryFile = "capture.mem"
)

// maxCapturedState bounds the state file a capture may publish. Measured at tens of
// kibibytes on v1.16.1; the bound is far above that, and exists because the VMM that
// writes the file is not trusted to stop.
const maxCapturedState = 64 << 20

// copyChunk is how much of a capture is copied between checks for cancellation.
const copyChunk = 64 << 20

// Capture snapshots a running microVM and publishes it under a snapshot name a
// later Restore can name.
//
// CAPTURE ONLY A GUEST YOU TRUST. Every fork restored from the snapshot resumes as
// that guest was, and a VMM that has been taken over writes the snapshot it likes.
// billet keeps such a snapshot from harming the host (see Restore); it cannot make it
// a good golden.
//
// THE GUEST IS LEFT PAUSED. Whether a captured state runs again is the caller's
// decision, because running it duplicates everything in it; Destroy is how it ends.
//
// PUBLISHED BY COPY, NEVER BY RENAME. The VMM wrote the state and memory into its
// jail as the account it runs as, and that account may still hold a descriptor to
// either. A file renamed out of the jail is the same inode, so a descriptor opened
// before would still write to what every later fork maps. A copy is an inode the
// jail has never seen, made by billet, read-only from the moment it is published.
//
// WHOLE OR NOT AT ALL. The copies and their compatibility record are written into a
// staging directory in the store and renamed into place in one step that refuses an
// existing name, so a restore never finds half of a snapshot and a capture never
// replaces one somebody may be running from.
//
// AND WITH ITS OWN SYSTEM DISK. A restored kernel's page cache describes the disk it
// was captured with, so while the guest is paused the root disk is flushed and
// published as the golden's (goldenDisk.PublishRoot), and the record names it. A
// publication of the memory that fails withdraws the disk, so the builder's
// teardown discards it as it would any clone.
//
// IT ANSWERS THE EVIDENCE of what it published, which the caller keeps for every
// Restore of the golden (see Evidence). A snapshot that is published and not yet
// durable answers its evidence with errPublishedNotDurable, because a restore can
// already find it.
func (p *Provider) Capture(ctx context.Context, id, snapshot string) (Evidence, error) {
	if err := checkSnapshotName(snapshot); err != nil {
		return Evidence{}, err
	}

	goldenName, err := goldenNameFor(id)
	if err != nil {
		return Evidence{}, err
	}

	goldens, err := p.goldens()
	if err != nil {
		return Evidence{}, err
	}

	lease, _ := provider.LeaseOf(id)

	unlock, err := p.lockLease(ctx, id)
	if err != nil {
		return Evidence{}, err
	}
	defer unlock()

	j, found, err := p.findJail(id)
	if err != nil {
		return Evidence{}, err
	}

	if !found {
		return Evidence{}, fmt.Errorf("firecracker: there is no microVM %s to capture", id)
	}

	owner, err := ownerOf(j)
	if err != nil {
		return Evidence{}, fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
	}

	if owner != p.owner {
		return Evidence{}, fmt.Errorf("firecracker: %s belongs to deployment %s, not this one", id,
			bounded(owner))
	}

	// THE VMM A RESTORE WOULD RUN. A jail started by another build is a snapshot no
	// restore here could load, so it is refused before the guest is paused.
	if j.execName != p.execName {
		return Evidence{}, fmt.Errorf("firecracker: %w: %s runs %s, and a restore here runs %s",
			errSnapshotIncompatible, id, bounded(j.execName), p.execName)
	}

	res, err := resourcesOf(j)
	if err != nil {
		return Evidence{}, err
	}

	shape, err := shapeOf(j)
	if err != nil {
		return Evidence{}, err
	}

	if shape.Image == "" || shape.Device == "" {
		return Evidence{}, fmt.Errorf("firecracker: %s was launched before billet recorded what it booted, "+
			"so its system disk cannot be published as a golden's", id)
	}

	guest, err := p.jailKernelDigest(j)
	if err != nil {
		return Evidence{}, err
	}

	// BEFORE THE PAUSE, so a host that cannot describe itself never stops a guest.
	record, err := p.hostCompatibility(guest)
	if err != nil {
		return Evidence{}, err
	}

	record.Deployment, record.Builder = p.owner, lease

	// THE BINARY THIS VMM IS RUNNING, NOT THE ONE AT ITS PATH NOW. A build replaced
	// under the same name while the guest ran would otherwise label this snapshot
	// with a VMM it was not taken by, and a restore with the new build would pass.
	//
	// A LABEL, NOT AN AUTHENTICATION. This catches an honest VMM whose binary was
	// upgraded under it. It does not try to prove which process sends the capture's
	// bytes against a VMM that has been taken over, because such a VMM already writes
	// every byte of the snapshot: the label would protect nothing it could not
	// already change. What contains a hostile snapshot is the restore side, which runs
	// billet's own build jailed and refuses a loaded guest of another shape.
	if err := p.checkRunningVMM(ctx, j, res, record.VMM); err != nil {
		return Evidence{}, err
	}

	record.VCPUs, record.Memory = shape.VCPUs, shape.Memory

	// THE STORE IS TAKEN BEFORE THE GUEST IS PAUSED, and the wait for it can be
	// abandoned: a capture queued behind another's multi-GiB copy holds this lease's
	// lock while it waits, and a guest paused for that long has stopped for nothing.
	store, err := p.openStore(ctx)
	if err != nil {
		return Evidence{}, err
	}
	defer store.close()

	// EVERY CONNECTION CHECKED FOR WHOSE VMM ANSWERS (newVMMAPIAs): this guest has
	// run, so the socket's name is no longer evidence of where it leads.
	if err := capture(ctx, p.apiAs(j.socket(), res.UID),
		snapshotFiles{State: "/" + captureStateFile, Memory: "/" + captureMemoryFile}); err != nil {
		return Evidence{}, err
	}

	// THE DISK, WHILE THE GUEST IS PAUSED: flushed, so the cluster holds every write
	// the memory remembers, then published in place.
	if err := p.flushRoot(shape.Device); err != nil {
		return Evidence{}, err
	}

	disk, err := goldens.PublishRoot(ctx, id, goldenName)
	if err != nil {
		return Evidence{}, err
	}

	record.Image, record.Disk = shape.Image, disk

	published, err := p.publishCapture(ctx, j, res, shape, store, snapshot, record)
	if err != nil {
		// A SNAPSHOT ALREADY VISIBLE KEEPS ITS DISK: a restore of it clones that disk,
		// so only a publication that never reached its name is withdrawn.
		if errors.Is(err, errPublishedNotDurable) {
			return published.evidence(), err
		}

		// ON A CONTEXT OF ITS OWN: the usual reason the publication failed is that
		// the caller's context ended.
		if withdrawErr := goldens.WithdrawRoot(context.WithoutCancel(ctx), disk, id); withdrawErr != nil {
			return Evidence{}, errors.Join(err, fmt.Errorf("firecracker: and the golden's system disk %s "+
				"stays published with no snapshot naming it: %w", disk, withdrawErr))
		}

		return Evidence{}, err
	}

	return published.evidence(), nil
}

// errPublishedNotDurable is a snapshot renamed into place whose directories could not
// be synced after: it is visible and may be restored, and may not survive a crash.
var errPublishedNotDurable = errors.New("the snapshot is published and not yet durable")

// heldStore is the snapshot store, open and locked for one publish.
type heldStore struct {
	path   string
	fd     int
	unlock func()
}

func (s heldStore) close() {
	s.unlock()
	_ = unix.Close(s.fd)
}

// openStore opens the snapshot store, creating it if need be, and takes its publish
// lock, giving up when ctx does.
func (p *Provider) openStore(ctx context.Context) (heldStore, error) {
	store := p.snapshotsDir()

	if err := os.MkdirAll(store, 0o755); err != nil {
		return heldStore{}, fmt.Errorf("firecracker: create the snapshot store %s: %w", store, err)
	}

	fd, err := openOwnedDir(unix.AT_FDCWD, store)
	if err != nil {
		return heldStore{}, err
	}

	unlock, err := lockStore(ctx, fd, store)
	if err != nil {
		return heldStore{}, errors.Join(err, unix.Close(fd))
	}

	return heldStore{path: store, fd: fd, unlock: unlock}, nil
}

// checkSnapshotName refuses a snapshot name that is not one plain directory entry.
func checkSnapshotName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("firecracker: %w: %s is not a plain snapshot name", errSnapshotNotPublished,
			bounded(name))
	}

	return nil
}

// checkRunningVMM refuses a microVM whose running VMM is not the build at execPath.
func (p *Provider) checkRunningVMM(ctx context.Context, j jail, res resources, want string) error {
	running, err := p.runningVMM(ctx, j, res)
	if err != nil {
		return err
	}
	defer running.Close()

	got, err := p.digests.ofOpen(running, "the running VMM")
	if err != nil {
		return fmt.Errorf("firecracker: digest the VMM %s is running: %w", j.id, err)
	}

	if got != want {
		return fmt.Errorf("firecracker: %w: %s is running a Firecracker build other than %s, which was "+
			"replaced while it ran", errSnapshotIncompatible, j.id, p.execPath)
	}

	return nil
}

// jailKernelDigest is the digest of the kernel linked into a jail, read through a
// descriptor that does not follow a link, and only if billet put it there.
//
// THE JAIL ROOT BELONGS TO THE JAILED ACCOUNT, which may rename anything in it, so the
// name alone proves nothing; a file billet's account owns is one that account cannot
// have made.
func (p *Provider) jailKernelDigest(j jail) (string, error) {
	path := filepath.Join(j.root(), guestKernel)

	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("firecracker: open the guest kernel of %s: %w", j.id, err)
	}

	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", fmt.Errorf("firecracker: read the guest kernel of %s: %w", j.id, err)
	}

	if !linkedByBillet(st) {
		return "", fmt.Errorf("firecracker: the guest kernel in the jail of %s is not the file billet "+
			"linked there", j.id)
	}

	return p.digests.ofOpen(f, path)
}

// publishCapture copies a capture out of its jail into the snapshot store.
//
// ONE PUBLISHER AT A TIME, under a lock on the store, which is also what makes a
// staging directory found there provably abandoned: a capture killed mid-copy leaves
// gigabytes under a random name no later capture would choose, so each one sweeps
// them first.
//
// It answers the record as published, with the digests of the files it copied.
func (p *Provider) publishCapture(
	ctx context.Context, j jail, res resources, shape guestShape, held heldStore, name string,
	record compatibility,
) (compatibility, error) {
	store, storeFD := held.path, held.fd

	p.sweepStaging(store)

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: name a staging directory: %w", err)
	}

	staging := stagingPrefix + hex.EncodeToString(suffix[:])
	stagingPath := filepath.Join(store, staging)

	if err := unix.Mkdirat(storeFD, staging, 0o700); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: stage the snapshot %s: %w", name, err)
	}

	// A FAILED PUBLISH TAKES ITS STAGING WITH IT, and says so if it cannot.
	fail := func(err error) (compatibility, error) {
		if rmErr := os.RemoveAll(stagingPath); rmErr != nil {
			return compatibility{}, errors.Join(err, fmt.Errorf("firecracker: remove the staged snapshot %s: %w",
				stagingPath, rmErr))
		}

		return compatibility{}, err
	}

	record, err := p.stage(ctx, j, res, shape, storeFD, staging, record)
	if err != nil {
		return fail(err)
	}

	if err := renameNoReplace(storeFD, staging, storeFD, name); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return fail(fmt.Errorf("firecracker: a snapshot named %s is already published, and billet "+
				"never replaces one: %w", name, err))
		}

		return fail(fmt.Errorf("firecracker: publish the snapshot %s: %w", name, err))
	}

	// THE NAME AND THE STORE BOTH DURABLE before success is reported: the store may
	// have been created by this call, and its own entry is in the directory above.
	if err := p.syncDir(store); err != nil {
		return record, fmt.Errorf("firecracker: %w: sync the published snapshot %s: %w", errPublishedNotDurable,
			name, err)
	}

	if err := p.syncDir(filepath.Dir(store)); err != nil {
		return record, fmt.Errorf("firecracker: %w: sync the snapshot store %s: %w", errPublishedNotDurable,
			store, err)
	}

	// THE JAIL'S COPIES GO, because each is the guest's whole memory again. Not a
	// failure of the capture if they will not: Destroy removes the jail with them.
	for _, file := range []string{captureStateFile, captureMemoryFile} {
		if err := os.Remove(filepath.Join(j.root(), file)); err != nil && !errors.Is(err, os.ErrNotExist) {
			p.log.Warn("left a captured file in its jail", "runner", j.id, "file", file, "error", err)
		}
	}

	return record, nil
}

// stage fills a staging directory with the capture's copies and its record, and
// answers the record with the digests of the copies.
func (p *Provider) stage(
	ctx context.Context, j jail, res resources, shape guestShape, storeFD int, staging string,
	record compatibility,
) (compatibility, error) {
	stagingFD, err := unix.Openat(storeFD, staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: open the staged snapshot %s: %w", staging, err)
	}
	defer unix.Close(stagingFD)

	root, err := unix.Open(j.root(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: open the jail root %s: %w", j.root(), err)
	}
	defer unix.Close(root)

	for _, c := range []struct {
		from, to string
		limit    int64
		digest   *string
	}{
		{captureStateFile, snapshotStateFile, maxCapturedState, &record.StateDigest},
		{captureMemoryFile, snapshotMemoryFile, shape.Memory, &record.MemoryDigest},
	} {
		digest, err := p.copyOut(ctx, root, stagingFD, c.from, c.to, c.limit, j, res)
		if err != nil {
			return compatibility{}, err
		}

		*c.digest = digest
	}

	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: encode the compatibility of %s: %w", j.id, err)
	}

	if err := p.writePublished(stagingFD, compatibilityFile, append(encoded, '\n')); err != nil {
		return compatibility{}, err
	}

	if err := unix.Fchmod(stagingFD, 0o755); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: open the staged snapshot %s for reading: %w", staging, err)
	}

	if err := unix.Fsync(stagingFD); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: sync the staged snapshot %s: %w", staging, err)
	}

	return record, nil
}

// copyOut copies one file the VMM wrote in its jail into a staged snapshot.
//
// JUDGED THROUGH ITS DESCRIPTOR, opened without following a link: the jailed account
// owns the directory, so the name could be anything by now. It must be a regular file
// the jailed account owns (jailWrote), no larger than limit, and the same file, the
// same size, after the copy as before it.
//
// BOUNDED, AND STOPPABLE. The VMM that wrote it is not trusted to stop, so no more
// than limit bytes are ever read, and the copy checks for cancellation between
// chunks rather than running to the end of a file billet did not size.
//
// It answers the SHA-256 of what it wrote, taken from the bytes as they were copied,
// so the digest is of the published file and costs no second read.
func (p *Provider) copyOut(
	ctx context.Context, root, staging int, from, to string, limit int64, j jail, res resources,
) (string, error) {
	fd, err := unix.Openat(root, from, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("firecracker: open the captured %s in the jail of %s: %w", from, j.id, err)
	}

	src := os.NewFile(uintptr(fd), filepath.Join(j.root(), from))
	defer src.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return "", fmt.Errorf("firecracker: read the captured %s of %s: %w", from, j.id, err)
	}

	if before.Mode&unix.S_IFMT != unix.S_IFREG || !p.jailWrote(before, res) {
		return "", fmt.Errorf("firecracker: the captured %s in the jail of %s is not a file its VMM wrote",
			from, j.id)
	}

	if before.Size > limit {
		return "", fmt.Errorf("firecracker: the captured %s of %s is %d bytes, more than the %d its guest "+
			"can hold", from, j.id, before.Size, limit)
	}

	dstFD, err := unix.Openat(staging, to,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return "", fmt.Errorf("firecracker: stage %s: %w", to, err)
	}

	dst := os.NewFile(uintptr(dstFD), to)
	defer dst.Close()

	hash := sha256.New()
	out := io.MultiWriter(dst, hash)

	var copied int64

	for copied < before.Size {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("firecracker: copying the captured %s of %s was canceled: %w", from, j.id, err)
		}

		n, err := io.CopyN(out, src, min(copyChunk, before.Size-copied))
		copied += n

		if err != nil {
			return "", fmt.Errorf("firecracker: copy the captured %s of %s: %w", from, j.id, err)
		}
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return "", fmt.Errorf("firecracker: re-read the captured %s of %s: %w", from, j.id, err)
	}

	if identityOf(from, &after) != identityOf(from, &before) {
		return "", fmt.Errorf("firecracker: the captured %s of %s changed while it was copied", from, j.id)
	}

	if err := p.sealPublished(dst, to); err != nil {
		return "", err
	}

	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

// writePublished writes a small file into a staged snapshot, sealed as the rest are.
func (p *Provider) writePublished(dir int, file string, content []byte) error {
	fd, err := unix.Openat(dir, file, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return fmt.Errorf("firecracker: stage %s: %w", file, err)
	}

	f := os.NewFile(uintptr(fd), file)
	defer f.Close()

	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("firecracker: write %s: %w", file, err)
	}

	return p.sealPublished(f, file)
}

// sealPublished makes a staged file read-only to everyone, then durable.
//
// THE MODE BEFORE THE SYNC, because the sync is what makes the inode's metadata
// durable: a mode changed after it could be lost to a crash, leaving a published
// file a restore refuses as not read-only.
func (p *Provider) sealPublished(f *os.File, file string) error {
	if err := f.Chmod(0o444); err != nil {
		return fmt.Errorf("firecracker: make %s read-only: %w", file, err)
	}

	if err := p.syncFile(f); err != nil {
		return fmt.Errorf("firecracker: sync %s: %w", file, err)
	}

	return nil
}

// stagingPrefix names a snapshot being published. checkSnapshotName refuses any name
// beginning with a dot, so no restore can name one.
const stagingPrefix = ".staging-"

// publishLock is the store's lock file. It stays in the store for good: a lock file
// removed while somebody waits on it lets the next publisher lock a new inode
// beside them.
const publishLock = ".publish.lock"

// lockStore takes the store's publish lock, polling so the wait ends when ctx does.
func lockStore(ctx context.Context, storeFD int, store string) (func(), error) {
	fd, err := unix.Openat(storeFD, publishLock, unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("firecracker: open the publish lock of %s: %w", store, err)
	}

	ticker := time.NewTicker(lifecycleLockPoll)
	defer ticker.Stop()

	for {
		err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Close(fd) }, nil
		}

		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return nil, errors.Join(fmt.Errorf("firecracker: lock the snapshot store %s: %w", store, err),
				unix.Close(fd))
		}

		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("firecracker: wait for the snapshot store %s: %w", store,
				ctx.Err()), unix.Close(fd))
		case <-ticker.C:
		}
	}
}

// sweepStaging removes staging directories abandoned by a publish that was killed.
// The caller holds the store's publish lock, so none of them is in use.
func (p *Provider) sweepStaging(store string) {
	entries, err := os.ReadDir(store)
	if err != nil {
		p.log.Warn("could not look for abandoned snapshot staging", "store", store, "error", err)

		return
	}

	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), stagingPrefix) {
			continue
		}

		if err := os.RemoveAll(filepath.Join(store, entry.Name())); err != nil {
			p.log.Warn("could not remove abandoned snapshot staging", "store", store,
				"staging", entry.Name(), "error", err)
		}
	}
}

// jailWritten reports whether a file in a jail is owned by the account its VMM runs as.
func jailWritten(st unix.Stat_t, res resources) bool {
	return res.UID > 0 && int(st.Uid) == res.UID
}

// linkedByBillet reports whether a file in a jail is one billet put there: a regular
// file owned by the account billet runs as, which the jailed account cannot create.
func linkedByBillet(st unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && int(st.Uid) == os.Geteuid()
}
