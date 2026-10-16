package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
)

// Capture snapshots a running microVM and publishes it under a snapshot name a
// later Restore can name.
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
func (p *Provider) Capture(ctx context.Context, id, snapshot string) error {
	if err := checkSnapshotName(snapshot); err != nil {
		return err
	}

	if _, ours := provider.LeaseOf(id); !ours {
		return fmt.Errorf("firecracker: %s does not name a lease, so it is not a microVM billet started",
			bounded(id))
	}

	unlock, err := p.lockLease(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()

	j, found, err := p.findJail(id)
	if err != nil {
		return err
	}

	if !found {
		return fmt.Errorf("firecracker: there is no microVM %s to capture", id)
	}

	owner, err := ownerOf(j)
	if err != nil {
		return fmt.Errorf("firecracker: read which deployment owns %s: %w", j.dir(), err)
	}

	if owner != p.owner {
		return fmt.Errorf("firecracker: %s belongs to deployment %s, not this one", id, bounded(owner))
	}

	// THE VMM A RESTORE WOULD RUN. A jail started by another build is a snapshot no
	// restore here could load, so it is refused before the guest is paused.
	if j.execName != p.execName {
		return fmt.Errorf("firecracker: %w: %s runs %s, and a restore here runs %s",
			errSnapshotIncompatible, id, bounded(j.execName), p.execName)
	}

	res, err := resourcesOf(j)
	if err != nil {
		return err
	}

	guest, err := p.jailKernelDigest(j)
	if err != nil {
		return err
	}

	// BEFORE THE PAUSE, so a host that cannot describe itself never stops a guest.
	record, err := p.hostCompatibility(guest)
	if err != nil {
		return err
	}

	if err := capture(ctx, p.apiFor(j.socket()),
		snapshotFiles{State: "/" + snapshotStateFile, Memory: "/" + snapshotMemoryFile}); err != nil {
		return err
	}

	return p.publishCapture(j, res, snapshot, record)
}

// checkSnapshotName refuses a snapshot name that is not one plain directory entry.
func checkSnapshotName(name string) error {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("firecracker: %w: %s is not a plain snapshot name", errSnapshotNotPublished,
			bounded(name))
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
func (p *Provider) publishCapture(j jail, res resources, name string, record compatibility) error {
	store := p.snapshotsDir()

	if err := os.MkdirAll(store, 0o755); err != nil {
		return fmt.Errorf("firecracker: create the snapshot store %s: %w", store, err)
	}

	storeFD, err := openOwnedDir(unix.AT_FDCWD, store)
	if err != nil {
		return err
	}
	defer unix.Close(storeFD)

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("firecracker: name a staging directory: %w", err)
	}

	staging := ".staging-" + hex.EncodeToString(suffix[:])

	if err := unix.Mkdirat(storeFD, staging, 0o700); err != nil {
		return fmt.Errorf("firecracker: stage the snapshot %s: %w", name, err)
	}

	published := false

	defer func() {
		if !published {
			_ = os.RemoveAll(filepath.Join(store, staging))
		}
	}()

	stagingFD, err := unix.Openat(storeFD, staging, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("firecracker: open the staged snapshot %s: %w", name, err)
	}
	defer unix.Close(stagingFD)

	root, err := unix.Open(j.root(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("firecracker: open the jail root %s: %w", j.root(), err)
	}
	defer unix.Close(root)

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := p.copyOut(root, stagingFD, file, j, res); err != nil {
			return err
		}
	}

	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("firecracker: encode the compatibility of %s: %w", name, err)
	}

	if err := writePublished(stagingFD, compatibilityFile, append(encoded, '\n')); err != nil {
		return err
	}

	if err := unix.Fchmod(stagingFD, 0o755); err != nil {
		return fmt.Errorf("firecracker: open the staged snapshot %s for reading: %w", name, err)
	}

	if err := unix.Fsync(stagingFD); err != nil {
		return fmt.Errorf("firecracker: sync the staged snapshot %s: %w", name, err)
	}

	if err := renameNoReplace(storeFD, staging, storeFD, name); err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			return fmt.Errorf("firecracker: a snapshot named %s is already published, and billet never "+
				"replaces one: %w", name, err)
		}

		return fmt.Errorf("firecracker: publish the snapshot %s: %w", name, err)
	}

	published = true

	if err := unix.Fsync(storeFD); err != nil {
		return fmt.Errorf("firecracker: sync the published snapshot %s: %w", name, err)
	}

	// THE JAIL'S COPIES GO, because each is the guest's whole memory again. Not a
	// failure of the capture if they will not: Destroy removes the jail with them.
	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := unix.Unlinkat(root, file, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			p.log.Warn("left a captured file in its jail", "runner", j.id, "file", file, "error", err)
		}
	}

	return nil
}

// copyOut copies one file the VMM wrote in its jail into a staged snapshot.
//
// JUDGED THROUGH ITS DESCRIPTOR, opened without following a link: the jailed account
// owns the directory, so the name could be anything by now. It must be a regular file
// the jailed account owns (jailWrote), and the same file, still the same size, after
// the copy as before it.
func (p *Provider) copyOut(root, staging int, file string, j jail, res resources) error {
	fd, err := unix.Openat(root, file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("firecracker: open the captured %s in the jail of %s: %w", file, j.id, err)
	}

	src := os.NewFile(uintptr(fd), filepath.Join(j.root(), file))
	defer src.Close()

	var before unix.Stat_t
	if err := unix.Fstat(fd, &before); err != nil {
		return fmt.Errorf("firecracker: read the captured %s of %s: %w", file, j.id, err)
	}

	if before.Mode&unix.S_IFMT != unix.S_IFREG || !p.jailWrote(before, res) {
		return fmt.Errorf("firecracker: the captured %s in the jail of %s is not a file its VMM wrote",
			file, j.id)
	}

	dstFD, err := unix.Openat(staging, file,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return fmt.Errorf("firecracker: stage %s: %w", file, err)
	}

	dst := os.NewFile(uintptr(dstFD), file)
	defer dst.Close()

	copied, err := io.Copy(dst, src)
	if err != nil {
		return fmt.Errorf("firecracker: copy the captured %s of %s: %w", file, j.id, err)
	}

	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return fmt.Errorf("firecracker: re-read the captured %s of %s: %w", file, j.id, err)
	}

	if copied != before.Size || identityOf(file, &after) != identityOf(file, &before) {
		return fmt.Errorf("firecracker: the captured %s of %s changed while it was copied", file, j.id)
	}

	return sealPublished(dst, file)
}

// writePublished writes a small file into a staged snapshot, sealed as the rest are.
func writePublished(dir int, file string, content []byte) error {
	fd, err := unix.Openat(dir, file, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o400)
	if err != nil {
		return fmt.Errorf("firecracker: stage %s: %w", file, err)
	}

	f := os.NewFile(uintptr(fd), file)
	defer f.Close()

	if _, err := f.Write(content); err != nil {
		return fmt.Errorf("firecracker: write %s: %w", file, err)
	}

	return sealPublished(f, file)
}

// sealPublished makes a staged file durable and read-only to everyone.
func sealPublished(f *os.File, file string) error {
	if err := f.Sync(); err != nil {
		return fmt.Errorf("firecracker: sync %s: %w", file, err)
	}

	if err := f.Chmod(0o444); err != nil {
		return fmt.Errorf("firecracker: make %s read-only: %w", file, err)
	}

	return nil
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
