package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"
)

type actionsVolumeManager interface {
	MountNew(ctx context.Context, device, target string) error
	MountReadOnly(ctx context.Context, device, target string) error
	// MountWritable mounts an existing filesystem for the node to write, as a
	// content-addressed cache volume is.
	MountWritable(ctx context.Context, device, target string) error
	Trim(ctx context.Context, target string) error
	// SyncWrites writes back the filesystem mounted at target and answers whether
	// everything written to it since it was mounted reached its volume: an error
	// wrapping store.ErrWritesLost when some never did, nil when nothing is mounted
	// there. A publication asks it before the unmount that comes before its
	// snapshot, because an unmount writes back without asking (writtenback.go).
	SyncWrites(ctx context.Context, target string) error
	Unmount(ctx context.Context, target string) error
	// Mounted reports whether something is mounted at target in this process's
	// mount namespace; an error is could-not-tell, never "no".
	Mounted(ctx context.Context, target string) (bool, error)
}

type hostActionsVolumeManager struct{}

// mountpointNotMounted is util-linux mountpoint's exit status for a directory that
// is not a mount point.
const mountpointNotMounted = 32

func (hostActionsVolumeManager) MountNew(ctx context.Context, device, target string) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("node: mkfs.ext4 is required for Actions cache archives: %w", err)
	}
	// NO DISCARD PASS. MountNew formats a volume Create has just made, a thin rbd
	// image with nothing allocated, so mke2fs's default of discarding every block
	// first gains nothing, and under load it was the whole cost: on 2026-10-03 the
	// format of a 20GiB Go cache volume was killed at "Discarding device blocks:
	// 0/5242880", and the session's cache answered "not mounted" to every request
	// of the build after it.
	if output, err := exec.CommandContext(ctx, mkfs, "-F", "-m", "0", "-E", "nodiscard",
		device).CombinedOutput(); err != nil {
		return fmt.Errorf("node: format Actions cache volume: %w: %s", err, boundedOutput(output))
	}

	return mountActionsVolume(ctx, device, target, writableMountOptions)
}

func (hostActionsVolumeManager) MountWritable(ctx context.Context, device, target string) error {
	return mountActionsVolume(ctx, device, target, writableMountOptions)
}

// writableMountOptions mount a volume the node writes.
//
// DATA THAT NEVER LANDED IS NEVER NAMED. Every device billet maps fails an OSD
// request after osd_request_timeout, and ext4's default (data_err=ignore) only logs
// a failed write of a file's data in ordered mode, so the journal can still commit
// a name for data that is not there; a restart, whose new mount has heard of no
// failure, would then publish it. data_err=abort aborts the journal instead, so the
// filesystem goes read-only (later writes fail, and SyncWrites reports it) and
// nothing committed names lost data. Measured 2026-11-13 on kernel 7.0 with the only
// OSD stopped: a file written during the outage was absent after the OSD returned
// and the volume was mounted again, the one written before it intact, e2fsck
// clean. A total outage aborts the journal either way, since its commit fails too;
// the option is for a partial one, where data and journal sit on different OSDs.
const writableMountOptions = "noatime,nodev,nosuid,noexec,data_err=abort"

func (hostActionsVolumeManager) MountReadOnly(ctx context.Context, device, target string) error {
	return mountActionsVolume(ctx, device, target, "ro,noload,nodev,nosuid,noexec")
}

func (hostActionsVolumeManager) Trim(ctx context.Context, target string) error {
	fstrim, err := exec.LookPath("fstrim")
	if err != nil {
		return fmt.Errorf("node: fstrim is required for staged Actions cache archives: %w", err)
	}
	if output, err := exec.CommandContext(ctx, fstrim, "--", target).CombinedOutput(); err != nil {
		return fmt.Errorf("node: trim staged Actions cache blocks: %w: %s", err, boundedOutput(output))
	}

	return nil
}

func mountActionsVolume(ctx context.Context, device, target, options string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return fmt.Errorf("node: create Actions cache mount point: %w", err)
	}
	mount, err := exec.LookPath("mount")
	if err != nil {
		return fmt.Errorf("node: mount is required for Actions cache archives: %w", err)
	}
	if output, err := exec.CommandContext(ctx, mount, "-t", "ext4", "-o", options,
		device, target).CombinedOutput(); err != nil {
		mountErr := fmt.Errorf("node: mount Actions cache volume: %w: %s", err, boundedOutput(output))

		return errors.Join(mountErr, undoFailedMount(ctx, target))
	}

	return nil
}

// failedMountUndoLimit bounds undoing a mount whose command failed, on a context
// of its own: the command's own context is often the reason it failed.
const failedMountUndoLimit = 30 * time.Second

// undoFailedMount leaves nothing mounted at target after a mount command failed.
//
// A FAILED COMMAND IS NOT A FAILED MOUNT. A mount killed by its context can have
// finished in the kernel: on 2026-11-06 one was killed and its Go cache volume
// stayed mounted in the node's namespace, so removing the mount point failed and
// the volume's discard could not unmap it, retrying for hours. So the kernel is
// asked, and what it holds is unmounted before the mount point goes.
func undoFailedMount(ctx context.Context, target string) error {
	undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failedMountUndoLimit)
	defer cancel()

	if err := (hostActionsVolumeManager{}).Unmount(undoCtx, target); err != nil {
		return fmt.Errorf("node: undo the failed Actions cache mount, which may still be mounted: %w", err)
	}

	return nil
}

func (hostActionsVolumeManager) Mounted(ctx context.Context, target string) (bool, error) {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("node: inspect Actions cache mount point: %w", err)
	}
	mountpoint, err := exec.LookPath("mountpoint")
	if err != nil {
		return false, fmt.Errorf("node: mountpoint is required for Actions cache recovery: %w", err)
	}
	// util-linux's mountpoint exits 32 for "not a mount point" and 1 for a failure
	// to look (2.41.3 on the reference deployment, measured 2026-10-03). Reading 1
	// as unmounted refused every directory a failed mount left behind, and the
	// session holding it never finished closing.
	if err := exec.CommandContext(ctx, mountpoint, "-q", "--", target).Run(); err != nil {
		exitErr, ok := errors.AsType[*exec.ExitError](err)
		if !ok || exitErr.ExitCode() != mountpointNotMounted {
			return false, fmt.Errorf("node: inspect Actions cache mount point: %w", err)
		}

		return false, nil
	}

	return true, nil
}

func (m hostActionsVolumeManager) SyncWrites(ctx context.Context, target string) error {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	mounted, err := m.Mounted(ctx, target)
	if err != nil || !mounted {
		return err
	}

	return syncFilesystem(target)
}

func (m hostActionsVolumeManager) Unmount(ctx context.Context, target string) error {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	mounted, err := m.Mounted(ctx, target)
	if err != nil {
		return err
	}
	if !mounted {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("node: remove stale Actions cache mount point: %w", err)
		}

		return nil
	}
	umount, err := exec.LookPath("umount")
	if err != nil {
		return fmt.Errorf("node: umount is required for Actions cache archives: %w", err)
	}
	if output, err := exec.CommandContext(ctx, umount, target).CombinedOutput(); err != nil {
		return fmt.Errorf("node: unmount Actions cache volume: %w: %s", err, boundedOutput(output))
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("node: remove Actions cache mount point: %w", err)
	}

	return nil
}

func boundedOutput(output []byte) string {
	const limit = 512
	if len(output) > limit {
		output = output[:limit]
	}

	return string(output)
}
