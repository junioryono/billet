package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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
//
// A WRITE-BACK THAT FAILED IS LOST WRITES, NOT A SLOW ONE. The device is opened, so
// the fsync's error is the kernel's report that pages it wrote back never reached the
// volume (an OSD request past the map's osd_request_timeout, a blocklisted client);
// those pages are clean again, and nothing writes them back a second time. The drive
// is still replaced, since whatever happens to the volume next needs it detached, and
// the answer is provider.ErrWritesLost, so it is discarded rather than published.
//
// ONLY THE DEVICE THE SLOT HOLDS: the recorded name is flushed only while the slot's
// node is that same block device, since a name the kernel gave this volume may by now
// be another's (a discard that unmapped it and failed after). Any other answer leaves
// the write-back to the close.
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

	if device != "" && !p.slotHolds(j.volumePath(slot), device) {
		device = ""
	}
	// A DETACH HANDED NO DEVICE IS NOT ONE WITH NOTHING TO HEAR: an earlier detach of
	// the slot that gave up while its write-back ran left that write-back's answer for
	// its device, and a detach whose proof of the device failed is handed none. The
	// device the slot's unfinished detach wrote back is asked again, while the slot
	// still holds that very block device.
	//
	// AND A SLOT WHOSE DEVICE CANNOT BE PROVED IS NOT TOLD SUCCESS: a write-back of its
	// device still in flight is waited on, never joined with a fresh open, and what it
	// lost reaches the slot itself (keepLoss), whatever the proof answers.
	if remembered := p.slotDevice(instanceID, slot); device == "" && remembered != "" {
		if p.slotHolds(j.volumePath(slot), remembered) {
			device = remembered
		} else if err := p.awaitWriteBack(ctx, remembered); err != nil {
			return fmt.Errorf("firecracker: wait for the write-back of cache slot %d of %s: %w",
				slot, instanceID, err)
		}
	}
	p.rememberSlotDevice(instanceID, slot, device)
	lost := p.flushBefore(ctx, device)
	if lost != nil && !errors.Is(lost, provider.ErrWritesLost) {
		return fmt.Errorf("firecracker: write cache slot %d of %s back before detaching it: %w",
			slot, instanceID, lost)
	}
	// A DETACH THAT FAILED KEPT WHAT IT HEARD FOR THE SLOT, and the next is told it
	// whatever device it is handed: the failed one put the placeholder in before its
	// PATCH, so the slot no longer holds the device, and a detach whose proof of the
	// device failed is handed none. The slot is this VM's until its detach succeeds.
	if slotLost := p.takeSlotLoss(instanceID, slot); lost == nil {
		lost = slotLost
	}

	// A DETACH THAT FAILED SAYS ONLY THAT IT FAILED: ErrWritesLost is the promise that
	// the volume is detached, so lost writes heard before a failure are kept for the
	// slot's next detach to be told.
	if err := p.replaceVolumePath(j, res, slot, ""); err != nil {
		p.keepSlotLoss(instanceID, slot, lost)

		return err
	}

	id := provider.VolumeSlotID(slot)
	if err := p.apiAs(j.socket(), res.UID).withTimeout(callerTime(ctx)).patch(ctx, "/drives/"+id,
		map[string]string{"drive_id": id, "path_on_host": "/" + id}); err != nil {
		p.keepSlotLoss(instanceID, slot, lost)

		return fmt.Errorf("firecracker: detach cache slot %d from %s: %w", slot, instanceID, err)
	}
	p.forgetSlotDevice(instanceID, slot)
	if lost != nil {
		return fmt.Errorf("firecracker: cache slot %d of %s is detached: %w", slot, instanceID, lost)
	}

	return nil
}

// flushBefore writes device back within ctx. ctx ending is an error, and so is a
// write-back that failed, as provider.ErrWritesLost, to every caller waiting on it: a
// device it cannot open is left to the VMM's close.
//
// ONE WRITE-BACK A DEVICE AT A TIME. Neither the open nor the fsync can be cancelled,
// so a caller whose time ends leaves them running; a detach of the same device after
// that waits on them rather than starting another beside them, so retries against a
// slow device add no descriptors and no blocked threads. Nothing is opened or written
// back under flushMu, so one slow device never holds another's detach.
//
// WRITTEN BACK ONLY FOR A CALLER STILL WAITING WHEN THE OPEN ENDS. The caller's proof
// of the device is fresh while it waits; once every caller has given up, the volume may
// be unmapped and its number given to another, so an open that ends then is closed
// again without a write-back. The write-back that runs holds the descriptor it opened,
// never a name it opens later, and while it holds it nothing on this host can prove the
// volume unheld, so no store detaches it.
//
// A FAILED OPEN ONLY MOVES THE WRITE-BACK INTO THE PATCH, which has the caller's time
// for it. A FAILED FLUSH IS LOST WRITES (DetachVolume).
//
// LOST WRITES ARE KEPT UNTIL A CALLER IS TOLD. The kernel tells the one fsync, and a
// write-back whose every caller gave up before it failed would otherwise tell nobody,
// and the next detach's own fsync would be told the volume is fine. So a failure is
// kept for the device until a caller receives it, and a later call is told it before
// anything else; only while it is the same disk, by the kernel's disk sequence
// number, since the name may by now be another volume's. A disk whose number cannot
// be read is told it, which discards a volume rather than publish one.
func (p *Provider) flushBefore(ctx context.Context, device string) error {
	if device == "" {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	seq, known := p.diskSeqOf(device)

	p.flushMu.Lock()
	wb, running := p.flushing[device]
	if !running {
		if lost := p.takeLossLocked(device, seq, known); lost != nil {
			p.flushMu.Unlock()

			return lost
		}
	}
	if !running {
		if p.flushing == nil {
			p.flushing = map[string]*writeBack{}
		}
		wb = &writeBack{opened: make(chan struct{}), done: make(chan struct{})}
		p.flushing[device] = wb
		go p.writeBack(device, wb)
	}
	select {
	case <-wb.opened:
	default:
		wb.waiting++
	}
	p.flushMu.Unlock()

	select {
	case <-wb.opened:
	case <-ctx.Done():
		p.flushMu.Lock()
		select {
		case <-wb.opened:
		default:
			wb.waiting--
		}
		p.flushMu.Unlock()

		return ctx.Err()
	}

	select {
	case <-wb.done:
		// A WRITE-BACK OF ANOTHER DISK IS NO ANSWER: one this caller joined may have
		// been of the disk that had the name before, whose loss is not this one's and
		// whose success says nothing of this one's writes.
		if wb.seq != "" && known && wb.seq != seq {
			return fmt.Errorf("firecracker: the write-back in flight for %s was of another disk", device)
		}
		if wb.lost != nil {
			p.flushMu.Lock()
			if loss, kept := p.unreported[device]; kept && errors.Is(loss.err, wb.lost) {
				delete(p.unreported, device)
			}
			p.flushMu.Unlock()
		}

		return wb.lost
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writesLoss is a device's lost writes no detach has been told yet, with the disk
// sequence number of the disk that lost them.
type writesLoss struct {
	seq string
	err error
}

// keepLoss keeps lost, if anything was lost, for the next detach of device to be told,
// as lost by the disk seq names (none known when it is empty), and for the next detach
// of every slot whose unfinished detach wrote device back, which is told it whatever
// device it is handed and whatever its proof of the device answers.
func (p *Provider) keepLoss(device, seq string, lost error) {
	if lost == nil || device == "" {
		return
	}

	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	if p.unreported == nil {
		p.unreported = map[string]writesLoss{}
	}
	p.unreported[device] = writesLoss{seq: seq, err: lost}
	for key, slotDevice := range p.slotDevices {
		if slotDevice != device {
			continue
		}
		if p.slotLosses == nil {
			p.slotLosses = map[string]error{}
		}
		p.slotLosses[key] = lost
	}
}

// awaitWriteBack waits, within ctx, for a write-back of device in flight to end,
// opening nothing; with none in flight it returns at once. What the write-back lost
// is kept for the slot that started it (keepLoss).
func (p *Provider) awaitWriteBack(ctx context.Context, device string) error {
	p.flushMu.Lock()
	wb, running := p.flushing[device]
	p.flushMu.Unlock()
	if !running {
		return nil
	}

	select {
	case <-wb.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func slotKey(instanceID string, slot int) string { return fmt.Sprintf("%s/%d", instanceID, slot) }

// keepSlotLoss keeps lost, if anything was lost, for the slot's next detach.
func (p *Provider) keepSlotLoss(instanceID string, slot int, lost error) {
	if lost == nil {
		return
	}

	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	if p.slotLosses == nil {
		p.slotLosses = map[string]error{}
	}
	p.slotLosses[slotKey(instanceID, slot)] = lost
}

// rememberSlotDevice records device as the one the slot's detach writes back; a
// detach handed no device keeps what an earlier one remembered.
func (p *Provider) rememberSlotDevice(instanceID string, slot int, device string) {
	if device == "" {
		return
	}

	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	if p.slotDevices == nil {
		p.slotDevices = map[string]string{}
	}
	p.slotDevices[slotKey(instanceID, slot)] = device
}

// forgetSlotDevice forgets the slot's device once its detach has succeeded.
func (p *Provider) forgetSlotDevice(instanceID string, slot int) {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	delete(p.slotDevices, slotKey(instanceID, slot))
}

// slotDevice is the device the slot's unfinished detach wrote back, if any.
func (p *Provider) slotDevice(instanceID string, slot int) string {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	return p.slotDevices[slotKey(instanceID, slot)]
}

// takeSlotLoss is the loss kept for the slot, taken out so it is told once.
func (p *Provider) takeSlotLoss(instanceID string, slot int) error {
	p.flushMu.Lock()
	defer p.flushMu.Unlock()

	lost := p.slotLosses[slotKey(instanceID, slot)]
	delete(p.slotLosses, slotKey(instanceID, slot))

	return lost
}

// takeLossLocked is the loss kept for device, if it is the disk seq names (or either's
// number is unknown), taken out under flushMu so it is told once. A loss kept for
// another disk that took the name is forgotten.
func (p *Provider) takeLossLocked(device, seq string, known bool) error {
	loss, kept := p.unreported[device]
	if !kept {
		return nil
	}
	delete(p.unreported, device)
	if known && loss.seq != "" && loss.seq != seq {
		return nil
	}

	return loss.err
}

func (p *Provider) diskSeqOf(device string) (string, bool) {
	if p.diskSeq == nil {
		return "", false
	}

	return p.diskSeq(device)
}

// readDiskSeq is a block device's disk sequence number, which the kernel gives each
// disk it creates and never gives another (Linux 5.15), so a number that matches is
// the same disk, never one that took its name.
func readDiskSeq(device string) (string, bool) {
	raw, err := os.ReadFile("/sys/class/block/" + filepath.Base(device) + "/diskseq")
	if err != nil {
		return "", false
	}
	seq := strings.TrimSpace(string(raw))

	return seq, seq != ""
}

// writeBack is one device's write-back in flight (flushBefore): opened when its open
// has ended, done when it has, waiting the callers still waiting for its open, and
// seq and lost, written before done closes, the disk sequence number of the disk it
// held open and what its failed fsync reported.
type writeBack struct {
	opened  chan struct{}
	done    chan struct{}
	waiting int
	seq     string
	lost    error
}

// writeBack opens device, writes it back if a caller is still waiting for it, and
// closes it.
func (p *Provider) writeBack(device string, wb *writeBack) {
	disk, err := p.openDisk(device)

	p.flushMu.Lock()
	wanted := wb.waiting > 0
	close(wb.opened)
	p.flushMu.Unlock()

	switch {
	case err != nil:
		p.log.Info("could not open a cache volume to write it back before its detach; the detach will",
			"device", device, "error", err)
	case !wanted:
		_ = disk.Close()
	default:
		// THE DISK IS NAMED WHILE ITS DESCRIPTOR IS HELD, so nothing can unmap it and give
		// the name to another first.
		wb.seq, _ = p.diskSeqOf(device)
		syncErr := disk.Sync()
		closeErr := disk.Close()
		if syncErr != nil {
			wb.lost = fmt.Errorf("%w: %s: %w", provider.ErrWritesLost, device, syncErr)
			// KEPT BEFORE DONE CLOSES, so no caller can miss it: one waiting is told and
			// takes it back out, and if none is, the next detach is told (flushBefore).
			p.keepLoss(device, wb.seq, wb.lost)
			p.log.Warn("a cache volume's write-back failed before its detach; what it lost is discarded, "+
				"never published", "device", device, "error", syncErr)
		} else if closeErr != nil {
			p.log.Info("could not close a cache volume after writing it back", "device", device, "error", closeErr)
		}
	}

	// DONE BEFORE FORGOTTEN, under the lock, so nothing can see the entry gone while
	// its write-back still runs.
	p.flushMu.Lock()
	close(wb.done)
	delete(p.flushing, device)
	p.flushMu.Unlock()
}

// sameBlockDevice says whether the node at slot, never followed, and device are one
// block device. Anything it cannot read is no.
func sameBlockDevice(slot, device string) bool {
	var held, named unix.Stat_t
	if unix.Lstat(slot, &held) != nil || unix.Lstat(device, &named) != nil {
		return false
	}

	return held.Mode&unix.S_IFMT == unix.S_IFBLK && named.Mode&unix.S_IFMT == unix.S_IFBLK &&
		held.Rdev == named.Rdev
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
