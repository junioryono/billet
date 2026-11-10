package ebss3

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
	"github.com/junioryono/billet/internal/store/blockfs"
)

// Host is the EC2 instance a self-attaching store runs on, read from its own instance
// metadata: the one instance it attaches its volumes to, and the zone they must be in.
type Host struct {
	InstanceID       string
	AvailabilityZone string
}

// attachAPI is the half of EC2 a self-attaching store adds: putting a volume on its own
// instance and taking it off again.
type attachAPI interface {
	AttachVolume(ctx context.Context, id, instance, point string) error
	DetachVolume(ctx context.Context, id, instance string) error
	Attachment(ctx context.Context, id string) (volumeAttachment, bool, error)
	AttachedTo(ctx context.Context, instance string) (map[string]time.Time, error)
}

// hostDisks is what a self-attaching store asks of this host's own block devices.
type hostDisks interface {
	// Find answers the device a volume is on here. found=false is a proof, from a
	// complete read, that no device here is the volume.
	Find(volume string) (device string, found bool, err error)
	// Unheld proves nothing on this host holds device (blockfs.Unheld).
	Unheld(device string) error
	// Verify proves device holds a clean ext4 filesystem (blockfs.VerifyExt4).
	Verify(ctx context.Context, device string) (storecontract.Filesystem, error)
}

// attachmentPoints are the device names a self-attaching store asks EC2 for, in order.
// On a Nitro instance the name is only a label: the volume appears as an NVMe device
// whose serial is its id. EC2 refuses a name already in use and one outside its naming
// scheme (`/dev/nvme5n1` was "not a valid EBS device name", 2026-11-09).
var attachmentPoints = func() []string {
	var points []string
	for c := 'f'; c <= 'z'; c++ {
		points = append(points, "/dev/sd"+string(c))
	}

	return points
}()

const (
	// deviceWait bounds how long an attached volume is waited for on the host. Its
	// device appeared within 0.6 s of AttachVolume's answer on 2026-11-09 (t3.small,
	// us-west-2), before EC2 described the attachment as complete.
	deviceWait = time.Minute
	// devicePoll is how often the host is asked meanwhile.
	devicePoll = 100 * time.Millisecond
	// unclaimedAttachmentAge is how long a volume attached to this instance must have
	// been attached before a sweep may take it for one no cache session names: a volume
	// being attached for a session is attached before the session records it.
	unclaimedAttachmentAge = 30 * time.Minute
)

// NewAttached constructs the store for a node that attaches its volumes to its own
// instance and hands them to its guests as block devices: a Firecracker node on AWS
// metal (ADR-030). host is the node's own instance, from its metadata; sessions names
// the volumes this node's cache sessions hold, so a sweep never takes one of them.
func NewAttached(
	cfg config.EBSS3Config,
	owner string,
	credentials CredentialSource,
	host Host,
	sessions func() (func(name string) bool, error),
) (*Store, error) {
	if !strings.HasPrefix(host.InstanceID, "i-") {
		return nil, fmt.Errorf("ebs-s3: %q is not an EC2 instance to attach cache volumes to", host.InstanceID)
	}
	if sessions == nil {
		return nil, errors.New("ebs-s3: a self-attaching store needs to know which volumes its sessions hold")
	}

	store, err := New(cfg, owner, credentials)
	if err != nil {
		return nil, err
	}

	// THE ZONE THE VOLUMES ARE MADE IN IS THE INSTANCE'S, or none of them attaches: EC2
	// answers InvalidVolume.ZoneMismatch (2026-11-09).
	if host.AvailabilityZone != store.cfg.AvailabilityZone {
		return nil, fmt.Errorf("ebs-s3: node.ebs_s3.availability_zone is %s, but this instance is in %s; a "+
			"volume attaches only in its own zone, so order this pool's hosts in %s or name the zone they are in",
			store.cfg.AvailabilityZone, host.AvailabilityZone, store.cfg.AvailabilityZone)
	}

	blocks, ok := store.blocks.(*ebsAPI)
	if !ok {
		return nil, errors.New("ebs-s3: a self-attaching store needs the EC2 client")
	}

	store.host = &host
	store.attach = blocks
	store.disks = sysDisks{root: "/sys/block"}
	store.sessions = sessions

	return store, nil
}

// local reports whether this store attaches its volumes to its own instance.
func (s Store) local() bool { return s.host != nil }

// attachLocal attaches a volume this store just created to its own instance and
// answers the device it appeared as.
//
// THE DEVICE IS PROVED BY ITS SERIAL, which an EBS volume's NVMe device reports as its
// id without the dash, never by the name asked for: on Nitro the name is a label, and
// the kernel numbers NVMe devices in the order it finds them.
func (s Store) attachLocal(ctx context.Context, volume string) (string, error) {
	attached := false
	for _, point := range attachmentPoints {
		err := s.attach.AttachVolume(ctx, volume, s.host.InstanceID, point)
		if err == nil {
			attached = true

			break
		}
		if attachmentPointInUse(err) {
			continue
		}

		return "", fmt.Errorf("ebs-s3: attach cache volume %s to this instance: %w", volume, err)
	}
	if !attached {
		return "", fmt.Errorf("ebs-s3: every device name billet asks for is in use on this instance, so "+
			"cache volume %s was not attached", volume)
	}

	deadline := time.NewTimer(deviceWait)
	defer deadline.Stop()

	poll := time.NewTicker(devicePoll)
	defer poll.Stop()

	for {
		device, found, err := s.disks.Find(volume)
		if err != nil {
			return "", fmt.Errorf("ebs-s3: find cache volume %s on this host: %w", volume, err)
		}
		if found {
			return device, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline.C:
			return "", fmt.Errorf("ebs-s3: cache volume %s was attached, and no device here reported its "+
				"serial within %s", volume, deviceWait)
		case <-poll.C:
		}
	}
}

// attachmentPointInUse reports EC2's refusal of a device name the instance already
// uses: InvalidParameterValue, "Invalid value '/dev/sdf' for unixDevice. Attachment point
// /dev/sdf is already in use" (2026-11-09). Only that refusal is retried under another
// name. The code alone is not enough: a name outside EC2's scheme is refused with the
// same code, as "not a valid EBS device name", and retrying that under every name would
// only hide it.
func attachmentPointInUse(err error) bool {
	apiErr, ok := errors.AsType[*awsAPIError](err)

	return ok && apiErr.Code == "InvalidParameterValue" && strings.Contains(apiErr.message, "is already in use")
}

// detachLocal takes one of this store's volumes off its own instance and waits until EC2
// describes it as available, on proof that nothing on this host holds it.
//
// NEVER WITHOUT THAT PROOF. EBS detached an ext4 volume mounted on the host without
// refusing, in 4.4 s, and the kernel aborted its journal (2026-11-09); a VMM serving the
// volume to a guest would lose it the same way. A volume attached to another instance is
// refused rather than touched.
func (s Store) detachLocal(ctx context.Context, volume string) error {
	attachment, found, err := s.attach.Attachment(ctx, volume)
	if err != nil {
		return fmt.Errorf("ebs-s3: ask where cache volume %s is attached: %w", volume, err)
	}
	if !found || attachment.Instance == "" {
		return nil
	}
	if attachment.Instance != s.host.InstanceID {
		return fmt.Errorf("ebs-s3: cache volume %s is attached to %s, not this instance, and is left there",
			volume, attachment.Instance)
	}

	device, present, err := s.disks.Find(volume)
	if err != nil {
		return fmt.Errorf("ebs-s3: find cache volume %s on this host before detaching it: %w", volume, err)
	}
	if present {
		if err := s.disks.Unheld(device); err != nil {
			return fmt.Errorf("ebs-s3: cache volume %s (%s) is not detached: %w", volume, device, err)
		}
	}

	if err := s.attach.DetachVolume(ctx, volume, s.host.InstanceID); err != nil {
		return fmt.Errorf("ebs-s3: detach cache volume %s: %w", volume, err)
	}

	blocks, ok := s.blocks.(*ebsAPI)
	if !ok {
		return s.awaitDetached(ctx, volume)
	}

	return blocks.waitVolume(ctx, volume, "available")
}

// awaitDetached waits until a volume is attached to nothing, for a store whose EC2
// client is a test's.
func (s Store) awaitDetached(ctx context.Context, volume string) error {
	poll := time.NewTicker(devicePoll)
	defer poll.Stop()

	for {
		attachment, found, err := s.attach.Attachment(ctx, volume)
		if err != nil {
			return err
		}
		if !found || attachment.Instance == "" {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
		}
	}
}

// releaseLocal is a failed attach's cleanup: the volume off this instance, if it got
// there, and deleted.
func (s Store) releaseLocal(ctx context.Context, volume string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unansweredVolumeLimit)
	defer cancel()

	if err := s.detachLocal(cleanupCtx, volume); err != nil {
		return fmt.Errorf("ebs-s3: cache volume %s is left attached for the sweep: %w", volume, err)
	}

	return s.blocks.DeleteVolume(cleanupCtx, volume)
}

// MappedDevice is the device a volume is on now. A remote store's volume is attached to
// a job's instance, and its id is its only name here; a self-attaching store's is proved
// by its serial on this host.
func (s Store) MappedDevice(ctx context.Context, volume storecontract.Volume) (string, error) {
	if !s.local() {
		return volume.Device, nil
	}
	if !strings.HasPrefix(volume.Handle, "vol-") {
		return "", fmt.Errorf("%w: %q is not an EBS volume", storecontract.ErrDeviceUnknown, volume.Handle)
	}

	device, found, err := s.disks.Find(volume.Handle)
	if err != nil {
		return "", fmt.Errorf("%w: %w", storecontract.ErrDeviceUnknown, err)
	}
	if found {
		return device, nil
	}

	_, exists, err := s.attach.Attachment(ctx, volume.Handle)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %w", storecontract.ErrDeviceUnknown, err)
	case !exists:
		return "", fmt.Errorf("%w: cache volume %s", storecontract.ErrVolumeGone, volume.Handle)
	default:
		return "", fmt.Errorf("%w: cache volume %s is on no device here", storecontract.ErrDeviceUnknown,
			volume.Handle)
	}
}

// SweepAttachments detaches and deletes this store's volumes that are attached to this
// instance and that no cache session names: a node that stopped between attaching a
// volume and recording it, or between a session's end and its discard. Only a volume
// attached for longer than unclaimedAttachmentAge is taken, and each only on proof that
// nothing on the host holds it. It answers the volumes it removed.
func (s Store) SweepAttachments(ctx context.Context) ([]string, error) {
	if !s.local() {
		return nil, nil
	}

	attached, err := s.attach.AttachedTo(ctx, s.host.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: list the cache volumes attached to this instance: %w", err)
	}

	named, err := s.sessions()
	if err != nil {
		return nil, fmt.Errorf("ebs-s3: read which volumes this node's cache sessions hold: %w", err)
	}

	var removed []string
	var errs []error
	now := s.now()
	for volume, since := range attached {
		if named(volume) || since.IsZero() || now.Sub(since) < unclaimedAttachmentAge {
			continue
		}
		if err := s.detachLocal(ctx, volume); err != nil {
			errs = append(errs, err)

			continue
		}
		if err := s.blocks.DeleteVolume(ctx, volume); err != nil {
			errs = append(errs, err)

			continue
		}
		removed = append(removed, volume)
	}

	return removed, errors.Join(errs...)
}

// sysDisks reads this host's block devices from sysfs.
type sysDisks struct {
	root string
}

// Find reads every block device's serial. An EBS volume's NVMe device reports its id
// without the dash ("vol0037520decafda6ab", 2026-11-09), so that is what is looked for.
func (d sysDisks) Find(volume string) (string, bool, error) {
	serial := strings.Replace(volume, "-", "", 1)

	entries, err := os.ReadDir(d.root)
	if err != nil {
		return "", false, fmt.Errorf("list block devices: %w", err)
	}

	var matches []string
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(d.root, entry.Name(), "device", "serial"))
		if err != nil {
			// A DEVICE WITH NO SERIAL IS NOT AN EBS VOLUME (a loop device, a partition
			// table's own entries), and one that went away was not this volume either.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return "", false, fmt.Errorf("read the serial of %s: %w", entry.Name(), err)
		}
		if strings.TrimSpace(string(body)) == serial {
			matches = append(matches, entry.Name())
		}
	}

	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return "/dev/" + matches[0], true, nil
	default:
		return "", false, fmt.Errorf("%d devices report %s's serial (%s)", len(matches), volume,
			strings.Join(matches, ", "))
	}
}

// Unheld is blockfs.Unheld.
func (sysDisks) Unheld(device string) error { return blockfs.Unheld(device) }

// Verify is blockfs.VerifyExt4.
func (sysDisks) Verify(ctx context.Context, device string) (storecontract.Filesystem, error) {
	return blockfs.VerifyExt4(ctx, device)
}
