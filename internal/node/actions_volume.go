package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

type actionsVolumeManager interface {
	MountNew(context.Context, string, string) error
	MountReadOnly(context.Context, string, string) error
	Unmount(context.Context, string) error
}

type hostActionsVolumeManager struct{}

func (hostActionsVolumeManager) MountNew(ctx context.Context, device, target string) error {
	mkfs, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("node: mkfs.ext4 is required for Actions cache archives: %w", err)
	}
	if output, err := exec.CommandContext(ctx, mkfs, "-F", "-m", "0", device).CombinedOutput(); err != nil {
		return fmt.Errorf("node: format Actions cache volume: %w: %s", err, boundedOutput(output))
	}

	return mountActionsVolume(ctx, device, target, "noatime,nodev,nosuid,noexec")
}

func (hostActionsVolumeManager) MountReadOnly(ctx context.Context, device, target string) error {
	return mountActionsVolume(ctx, device, target, "ro,noload,nodev,nosuid,noexec")
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
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(mountErr,
				fmt.Errorf("node: remove failed Actions cache mount point: %w", removeErr))
		}

		return mountErr
	}

	return nil
}

func (hostActionsVolumeManager) Unmount(ctx context.Context, target string) error {
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("node: inspect Actions cache mount point: %w", err)
	}
	mountpoint, err := exec.LookPath("mountpoint")
	if err != nil {
		return fmt.Errorf("node: mountpoint is required for Actions cache recovery: %w", err)
	}
	mounted := true
	if err := exec.CommandContext(ctx, mountpoint, "-q", "--", target).Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
			return fmt.Errorf("node: inspect Actions cache mount point: %w", err)
		}
		mounted = false
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
