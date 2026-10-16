package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/provider"
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

// errSnapshotNotPublished is a snapshot that is missing, or that is not in the
// shape a jailed VMM may be given.
var errSnapshotNotPublished = errors.New("not a published snapshot")

// placeSnapshot links a published snapshot's state and memory into a jail.
//
// THE KERNEL'S RULE, FOR THE SAME REASON (see build). A hard link is the same inode,
// so chowning the name inside the jail would hand the one copy every fork maps to
// the account this VMM runs as, and it could rewrite the memory every later fork of
// that golden resumes from. So the links are placed after the chroot is chowned and
// are never chowned themselves, and the source must already be owned by billet and
// read-only: a jailed VMM reads it, nothing it runs as can write it.
//
// AND NEVER BY COPY. A copy would lose the shared page cache and write gigabytes per
// restore, and a source on another filesystem than the jails is a deployment billet
// refuses rather than one it quietly makes slow.
func (p *Provider) placeSnapshot(j jail, name string) error {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("firecracker: %w: %s is not a plain snapshot name", errSnapshotNotPublished,
			bounded(name))
	}

	for _, file := range []string{snapshotStateFile, snapshotMemoryFile} {
		src := filepath.Join(p.snapshotsDir(), name, file)

		if err := checkPublished(src); err != nil {
			return err
		}

		dst := filepath.Join(j.root(), file)

		if err := os.Link(src, dst); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				return fmt.Errorf("firecracker: %s is on another filesystem than the jails under %s, "+
					"so it cannot be linked into one, and billet does not copy a snapshot: %w",
					src, p.cfg.ChrootBase, err)
			}

			return fmt.Errorf("firecracker: link %s into the jail of %s: %w", src, j.id, err)
		}

		// THE SAME FILE, STILL IN THE SAME SHAPE, read through the link the VMM will
		// open, because a check of the source alone says nothing about what the jail
		// now holds.
		if err := samePublished(src, dst); err != nil {
			return err
		}
	}

	return nil
}

// checkPublished proves a snapshot file is one a jailed VMM may read and never write.
//
// FROM WHAT IS PROVED, NOT WHAT IS PRESENT: a regular file, not a link to one,
// owned by the account billet runs as, and mode 0444 exactly. Anything else is
// refused rather than repaired, because repairing it would be billet deciding that a
// file someone else could write is the snapshot it published.
func checkPublished(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("firecracker: %w: %s: %w", errSnapshotNotPublished, path, err)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("firecracker: %w: %s is a %s, not a regular file", errSnapshotNotPublished,
			path, info.Mode().Type())
	}

	if perm := info.Mode().Perm(); perm != 0o444 {
		return fmt.Errorf("firecracker: %w: %s has mode %04o, and a published snapshot is 0444",
			errSnapshotNotPublished, path, perm)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("firecracker: %w: could not tell who owns %s", errSnapshotNotPublished, path)
	}

	if uid := os.Geteuid(); int(st.Uid) != uid {
		return fmt.Errorf("firecracker: %w: %s is owned by uid %d, not by the account billet runs as (%d)",
			errSnapshotNotPublished, path, st.Uid, uid)
	}

	return nil
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
