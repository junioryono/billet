package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

var _ provider.VolumeAttacher = (*Provider)(nil)

// AttachVolume replaces one reserved drive slot with a mapped block device.
//
// Firecracker supports this after boot only for a cooperative guest: the guest
// must not have mounted or accessed the placeholder. The sticky-disk action waits
// for this method to return before it formats or mounts the device.
func (p *Provider) AttachVolume(
	ctx context.Context,
	instanceID string,
	slot int,
	device string,
) error {
	j, res, err := p.volumeJail(instanceID, slot)
	if err != nil {
		return err
	}

	if err := p.replaceVolumePath(j, res, slot, device); err != nil {
		return err
	}

	// TO THIS MICROVM'S VMM AND NO OTHER (newVMMAPIAs): the guest has run, so the
	// socket's name is no longer evidence of whose VMM it reaches.
	id := provider.VolumeSlotID(slot)
	if err := p.apiAs(j.socket(), res.UID).patch(ctx, "/drives/"+id, map[string]string{
		"drive_id": id, "path_on_host": "/" + id,
	}); err != nil {
		return fmt.Errorf("firecracker: attach cache slot %d to %s: %w", slot, instanceID, err)
	}

	return nil
}

// DetachVolume replaces a drive with an empty placeholder before storage unmaps it.
//
// A successful PATCH is the boundary: only then has the VMM closed the device and
// may the store snapshot or unmap it. A failed patch leaves the device mapped and
// returns an error so the caller cannot cross that boundary by accident.
//
// THE DEVICE IS WRITTEN BACK FIRST, ON THE CALLER'S TIME. The guest's writes are in
// the host's page cache (flushDevice), and the VMM's close of the device writes them
// all back before it answers the PATCH: 1.5 GB took 11.2 s on a 100 GB gp3, past
// apiTimeout, so the drive was never replaced and the store never published
// (c7g.metal, 2026-11-13, #871). Flushed first, the PATCH took 0.2 s. A flush that
// cannot be made is not the detach's failure, since the close writes back anyway, and
// the PATCH has what is left of the caller's time for whatever the guest wrote since.
func (p *Provider) DetachVolume(
	ctx context.Context,
	instanceID string,
	slot int,
	device string,
) error {
	j, res, err := p.volumeJail(instanceID, slot)
	if err != nil {
		return err
	}

	if err := p.flushBefore(ctx, device); err != nil {
		return fmt.Errorf("firecracker: write cache slot %d of %s back before detaching it: %w",
			slot, instanceID, err)
	}

	if err := p.replaceVolumePath(j, res, slot, ""); err != nil {
		return err
	}

	id := provider.VolumeSlotID(slot)
	if err := p.apiAs(j.socket(), res.UID).withTimeout(callerTime(ctx)).patch(ctx, "/drives/"+id,
		map[string]string{"drive_id": id, "path_on_host": "/" + id}); err != nil {
		return fmt.Errorf("firecracker: detach cache slot %d from %s: %w", slot, instanceID, err)
	}

	return nil
}

// flushBefore writes device back within ctx. Only ctx ending is an error: a device it
// cannot open or flush is left to the VMM's close.
func (p *Provider) flushBefore(ctx context.Context, device string) error {
	if device == "" {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A FAILED FLUSH ONLY MOVES THE WRITE-BACK INTO THE PATCH, which has the
		// caller's time for it.
		if err := p.flushDisk(device); err != nil {
			p.log.Info("could not write a cache volume back before its detach; the detach will",
				"device", device, "error", err)
		}
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// callerTime is how long a call may take within ctx: what is left of its deadline,
// never less than apiTimeout, and apiTimeout when it has none.
func callerTime(ctx context.Context) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return apiTimeout
	}

	return max(time.Until(deadline), apiTimeout)
}

func (p *Provider) volumeJail(instanceID string, slot int) (jail, resources, error) {
	if slot < 0 || slot >= provider.MaxVolumes {
		return jail{}, resources{}, fmt.Errorf("firecracker: cache slot %d is outside 0-%d",
			slot, provider.MaxVolumes-1)
	}

	j, found, err := p.findJail(instanceID)
	if err != nil {
		return jail{}, resources{}, err
	}

	if !found {
		return jail{}, resources{}, fmt.Errorf("firecracker: no microVM named %s", instanceID)
	}

	owner, err := ownerOf(j)
	if err != nil {
		return jail{}, resources{}, fmt.Errorf("firecracker: read the owner of %s: %w", instanceID, err)
	}

	if owner != p.owner {
		return jail{}, resources{}, fmt.Errorf("firecracker: microVM %s belongs to another deployment",
			instanceID)
	}

	if _, err := os.Lstat(j.volumePath(slot)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return jail{}, resources{}, fmt.Errorf("firecracker: microVM %s did not reserve cache slot %d",
				instanceID, slot)
		}

		return jail{}, resources{}, fmt.Errorf("firecracker: inspect cache slot %d for %s: %w",
			slot, instanceID, err)
	}

	res, err := resourcesOf(j)
	if err != nil {
		return jail{}, resources{}, err
	}

	if res.UID <= 0 || res.GID <= 0 {
		return jail{}, resources{}, fmt.Errorf("firecracker: microVM %s has no recorded jail identity",
			instanceID)
	}

	return j, res, nil
}

func (p *Provider) replaceVolumePath(j jail, res resources, slot int, device string) error {
	id := provider.VolumeSlotID(slot)
	staged, err := os.CreateTemp(j.root(), "."+id+"-")
	if err != nil {
		return fmt.Errorf("firecracker: stage cache slot %d for %s: %w", slot, j.id, err)
	}

	stagedPath := staged.Name()
	defer os.Remove(stagedPath)

	if err := staged.Close(); err != nil {
		return fmt.Errorf("firecracker: close staged cache slot %d for %s: %w", slot, j.id, err)
	}

	if device != "" {
		if err := os.Remove(stagedPath); err != nil {
			return fmt.Errorf("firecracker: prepare cache device node for %s: %w", j.id, err)
		}

		if err := p.mknod(stagedPath, device, res.UID, res.GID); err != nil {
			return err
		}
	} else if err := p.chownOne(stagedPath, res.UID, res.GID); err != nil {
		return fmt.Errorf("firecracker: give cache placeholder to uid %d: %w", res.UID, err)
	}

	if err := os.Rename(stagedPath, filepath.Join(j.root(), id)); err != nil {
		return fmt.Errorf("firecracker: replace cache slot %d for %s: %w", slot, j.id, err)
	}

	return nil
}
