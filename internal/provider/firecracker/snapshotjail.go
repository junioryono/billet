package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/regularfile"
)

// The files of a published snapshot, under its own directory and inside a jail.
const (
	snapshotStateFile  = "snapshot.state"
	snapshotMemoryFile = "snapshot.mem"
)

// snapshotsDir is where the snapshots this node may restore are published.
//
// BESIDE THE JAILS, ON THEIR FILESYSTEM, so a restore can hard-link a snapshot into
// a jail rather than copy it: a copy writes the guest's whole memory again for every
// workspace and gives up the page cache the forks share (measured: ten forks were
// 227 MiB PSS together because they map one file). And its name holds no
// "firecracker", so execDirs never takes it for a directory a jailer built.
func (p *Provider) snapshotsDir() string {
	return filepath.Join(p.cfg.ChrootBase, "billet-snapshots")
}

// errSnapshotNotPublished is a snapshot that is absent, or that is not in the shape
// a jailed VMM may be given. An inspection that could not answer is not this: it is
// returned as itself, because "could not tell" is not "no".
var errSnapshotNotPublished = errors.New("not a published snapshot")

// placeSnapshot links a published snapshot's state and memory into a jail, once its
// compatibility record says this host, this VMM and the guest kernel this restore
// resolved (kernel) are the ones it was taken with.
//
// THE KERNEL'S RULE, FOR THE SAME REASON (see build). A hard link is the same inode,
// so chowning the name inside the jail would hand the one copy every fork maps to
// the account this VMM runs as, and it could rewrite the memory every later fork of
// that golden resumes from. So the links are placed after the chroot is chowned and
// are never chowned themselves, and the source must already be owned by billet and
// read-only: a jailed VMM reads it, nothing it runs as can write it.
//
// THROUGH DESCRIPTORS, NEVER PATHNAMES. Each directory on the way is opened without
// following a symlink and proved to be billet's and writable by no one else, the
// file is judged through its own descriptor, and the link is made relative to the
// directories already held. Only billet's account can rename anything in a directory
// proved that way, so the name linked is the file judged, and the link is checked
// against that file's inode afterwards.
//
// AND NEVER BY COPY. A copy would lose the shared page cache and write gigabytes per
// restore, and a source on another filesystem than the jails is a deployment billet
// refuses rather than one it quietly makes slow.
func (p *Provider) placeSnapshot(j jail, name, kernel string) error {
	if err := checkSnapshotName(name); err != nil {
		return err
	}

	store, err := openOwnedDir(unix.AT_FDCWD, p.snapshotsDir())
	if err != nil {
		return err
	}
	defer unix.Close(store)

	dirFD, err := openOwnedDir(store, name)
	if err != nil {
		return fmt.Errorf("firecracker: the snapshot %s: %w", name, err)
	}

	dir := os.NewFile(uintptr(dirFD), filepath.Join(p.snapshotsDir(), name))
	defer dir.Close()

	// WHERE IT CAN RUN, BEFORE ANYTHING IS LINKED: a snapshot taken on another CPU,
	// host kernel, Firecracker build or guest kernel is refused here, because the
	// VMM would load it and the guest would misbehave after it resumed.
	if err := p.checkCompatibility(dir, kernel); err != nil {
		return err
	}

	root, err := unix.Open(j.root(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("firecracker: open the jail root %s: %w", j.root(), err)
	}
	defer unix.Close(root)

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		if err := linkPublished(dir, root, file, filepath.Join(dir.Name(), file), j.id); err != nil {
			return err
		}
	}

	return nil
}

// linkPublished judges one published file through its descriptor, links it into the
// jail root, and proves the link is that file.
//
// OPENED FOR ITS IDENTITY ALONE (regularfile.OpenIdentityAt: O_PATH on Linux), so a
// device someone published by mistake is refused without being opened: an open is an
// operation on a device, and a watchdog is armed by one.
func linkPublished(dir *os.File, root int, file, path, jailID string) error {
	id, err := regularfile.OpenIdentityAt(dir, file)
	if err != nil {
		return refusalOrUnknown(int(dir.Fd()), file, path, err)
	}
	defer id.Close()

	var src unix.Stat_t
	if err := unix.Fstat(int(id.Fd()), &src); err != nil {
		return fmt.Errorf("firecracker: could not tell whether %s is published: %w", path, err)
	}

	if err := judgePublished(src, path); err != nil {
		return err
	}

	if err := unix.Linkat(int(dir.Fd()), file, root, file, 0); err != nil {
		if errors.Is(err, unix.EXDEV) {
			return fmt.Errorf("firecracker: %s is on another filesystem than the jails, so it cannot "+
				"be linked into one, and billet does not copy a snapshot: %w", path, err)
		}

		return fmt.Errorf("firecracker: link %s into the jail of %s: %w", path, jailID, err)
	}

	// THE SAME FILE, STILL IN THE SAME SHAPE, read through the name the VMM will
	// open, because a check of the source alone says nothing about what the jail
	// now holds.
	var linked unix.Stat_t
	if err := unix.Fstatat(root, file, &linked, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("firecracker: could not tell what was linked into the jail of %s: %w", jailID, err)
	}

	if linked.Dev != src.Dev || linked.Ino != src.Ino {
		return fmt.Errorf("firecracker: %w: %s in the jail of %s is not the file published at %s",
			errSnapshotNotPublished, file, jailID, path)
	}

	return judgePublished(linked, path)
}

// openOwnedDir opens a directory without following a symlink and proves it is
// owned by the account billet runs as and writable by no one else.
func openOwnedDir(at int, name string) (int, error) {
	fd, err := unix.Openat(at, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, refusalOrUnknown(at, name, name, err)
	}

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)

		return -1, fmt.Errorf("firecracker: could not tell who owns %s: %w", name, err)
	}

	if uid := os.Geteuid(); int(st.Uid) != uid {
		unix.Close(fd)

		return -1, fmt.Errorf("firecracker: %w: %s is owned by uid %d, not by the account billet runs as (%d)",
			errSnapshotNotPublished, name, st.Uid, uid)
	}

	if perm := os.FileMode(st.Mode).Perm(); perm&0o022 != 0 {
		unix.Close(fd)

		return -1, fmt.Errorf("firecracker: %w: %s has mode %04o, so someone other than billet could "+
			"replace what it holds", errSnapshotNotPublished, name, perm)
	}

	return fd, nil
}

// refusalOrUnknown says which of the two an open's failure was: absent, or a symlink
// or non-directory where one was required, is a snapshot that is not published;
// anything else is an inspection that could not answer.
//
// A SYMLINK IS NAMED AS ONE ON EVERY PLATFORM. O_NOFOLLOW on a symlink fails with
// ELOOP, but with O_DIRECTORY as well darwin answers ENOTDIR instead, so the name
// itself is looked at, not followed, before the refusal says what it found.
func refusalOrUnknown(at int, name, path string, err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return fmt.Errorf("firecracker: %w: %s does not exist", errSnapshotNotPublished, path)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		var st unix.Stat_t
		if statErr := unix.Fstatat(at, name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil &&
			st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fmt.Errorf("firecracker: %w: %s is a symlink, and billet judges a snapshot as itself",
				errSnapshotNotPublished, path)
		}

		return fmt.Errorf("firecracker: %w: %s is not what a published snapshot holds there",
			errSnapshotNotPublished, path)
	default:
		return fmt.Errorf("firecracker: could not tell whether %s is published: %w", path, err)
	}
}

// judgePublished proves one stat is of a file a jailed VMM may read and never write.
//
// FROM WHAT IS PROVED, NOT WHAT IS PRESENT: a regular file, owned by the account
// billet runs as, and mode 0444 exactly. Anything else is refused rather than
// repaired, because repairing it would be billet deciding that a file someone else
// could write is the snapshot it published.
func judgePublished(st unix.Stat_t, path string) error {
	mode := os.FileMode(st.Mode & 0o777)

	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
	case unix.S_IFLNK:
		return fmt.Errorf("firecracker: %w: %s is a symlink, and billet judges a snapshot as itself",
			errSnapshotNotPublished, path)
	default:
		return fmt.Errorf("firecracker: %w: %s is not a regular file", errSnapshotNotPublished, path)
	}

	if mode.Perm() != 0o444 {
		return fmt.Errorf("firecracker: %w: %s has mode %04o, and a published snapshot is 0444",
			errSnapshotNotPublished, path, mode.Perm())
	}

	if uid := os.Geteuid(); int(st.Uid) != uid {
		return fmt.Errorf("firecracker: %w: %s is owned by uid %d, not by the account billet runs as (%d)",
			errSnapshotNotPublished, path, st.Uid, uid)
	}

	return nil
}

// checkPublished judges a file by pathname for a caller that holds no directory
// descriptor: the entry is opened for its identity alone, never followed.
func checkPublished(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("firecracker: could not tell whether %s is published: %w", path, err)
	}
	defer dir.Close()

	id, err := regularfile.OpenIdentityAt(dir, filepath.Base(path))
	if err != nil {
		return refusalOrUnknown(int(dir.Fd()), filepath.Base(path), path, err)
	}
	defer id.Close()

	var st unix.Stat_t
	if err := unix.Fstat(int(id.Fd()), &st); err != nil {
		return fmt.Errorf("firecracker: could not tell whether %s is published: %w", path, err)
	}

	return judgePublished(st, path)
}

// samePublished proves a link in a jail is the published file, still read-only.
func samePublished(src, dst string) error {
	if err := checkPublished(dst); err != nil {
		return err
	}

	a, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("firecracker: re-read %s: %w", src, err)
	}

	b, err := os.Lstat(dst)
	if err != nil {
		return fmt.Errorf("firecracker: read %s: %w", dst, err)
	}

	if !os.SameFile(a, b) {
		return fmt.Errorf("firecracker: %w: %s is not the file published at %s", errSnapshotNotPublished,
			dst, src)
	}

	return nil
}

// restoreInto loads the snapshot placed in this jail into its freshly started VMM
// and resumes it with this lease's metadata.
func (p *Provider) restoreInto(
	ctx context.Context, api *vmmAPI, spec provider.Spec, tap string,
) (*Instance, error) {
	md, err := metadata(spec)
	if err != nil {
		return nil, err
	}

	if err := restore(ctx, api, restoreSpec{
		ID: spec.Name,
		// PATHS INSIDE THE CHROOT, where placeSnapshot linked them.
		Files: snapshotFiles{State: "/" + snapshotStateFile, Memory: "/" + snapshotMemoryFile},
		Tap:   tap,
		// Metadata is this lease's, never the golden's: the data store is empty after
		// a load, so nothing of the golden's survives into it.
		Metadata: md,
	}); err != nil {
		return nil, err
	}

	return &Instance{ID: spec.Name, Name: spec.Name, Running: true}, nil
}

// checkCompatibility refuses a snapshot whose record does not match this host.
func (p *Provider) checkCompatibility(dir *os.File, kernel string) error {
	record, err := readCompatibility(dir, filepath.Join(dir.Name(), compatibilityFile))
	if err != nil {
		return err
	}

	guest, err := p.digests.of(kernel)
	if err != nil {
		return fmt.Errorf("firecracker: could not tell where %s can be restored: digest the guest kernel "+
			"%s: %w", dir.Name(), kernel, err)
	}

	host, err := p.hostCompatibility(guest)
	if err != nil {
		return fmt.Errorf("firecracker: could not tell where %s can be restored: %w", dir.Name(), err)
	}

	if diff := record.mismatch(host); diff != "" {
		return fmt.Errorf("firecracker: %w: %s differs in %s", errSnapshotIncompatible, dir.Name(), diff)
	}

	return nil
}
