package ec2

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

const (
	ebsAttachmentPoll = 2 * time.Second
	// attachRunningTimeout bounds how long an attach waits for the instance to
	// reach 'running'. Long enough to ride out the pending->running window, short
	// enough that a genuinely stuck instance falls back cold rather than stalling
	// the job's cache request.
	attachRunningTimeout = 2 * time.Minute
)

var (
	_ provider.VolumeAttacher     = (*Provider)(nil)
	_ provider.GuestVolumeLocator = (*Provider)(nil)
)

func attachmentDevice(slot int) (string, error) {
	if slot < 0 || slot >= provider.MaxVolumes {
		return "", fmt.Errorf("ec2: cache volume slot %d is outside 0..%d", slot, provider.MaxVolumes-1)
	}

	return "/dev/sd" + string(rune('f'+slot)), nil
}

// GuestVolumeDevice names the persistent udev link created for an EBS NVMe
// device. The API's /dev/sdX attachment name is not the name Nitro exposes.
func (p *Provider) GuestVolumeDevice(_ int, volume string) string {
	return "/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_" + strings.ReplaceAll(volume, "-", "")
}

// AttachVolume hot-attaches one owned EBS cache volume and waits for AWS to
// report the attachment complete before the guest is told to discover it.
func (p *Provider) AttachVolume(
	ctx context.Context,
	instanceName string,
	slot int,
	volume string,
) error {
	device, err := attachmentDevice(slot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" || !strings.HasPrefix(volume, "vol-") {
		return errors.New("ec2: a cache attachment needs an instance name and an EBS volume id")
	}
	instance, found, err := p.Find(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("ec2: resolve cache instance %s: %w", instanceName, err)
	}
	if !found {
		return fmt.Errorf("ec2: no running owned instance named %s", instanceName)
	}
	if err := p.requireOwnedCacheVolume(ctx, volume); err != nil {
		return err
	}
	attachCtx, cancel := context.WithTimeout(ctx, attachRunningTimeout)
	defer cancel()
	if err := p.attachRunningVolume(attachCtx, instance.ID, volume, device); err != nil {
		return fmt.Errorf("ec2: attach cache volume %s to %s: %w", volume, instanceName, err)
	}

	return p.waitCacheVolume(ctx, volume, func(item cacheVolumeItem) bool {
		for _, attachment := range item.Attachments {
			if attachment.InstanceID == instance.ID && attachment.Device == device &&
				attachment.State == "attached" {
				return true
			}
		}

		return false
	})
}

// attachRunningVolume issues AttachVolume, waiting out the pending->running
// window. EC2's control plane can still report an instance 'pending' for a short
// interval after its guest has booted and the runner is already asking for
// cache, and AttachVolume refuses with IncorrectState until the state catches
// up. Retrying rather than falling back cold is what keeps a producer job from
// losing its warm image store to a boot-time race: an attach that loses the race
// leaves the seed job cold, nothing is persisted, and the consumer's
// `docker run --pull=never` then fails on the absent image.
//
// ONLY IncorrectState is retried, which is what keeps the retry safe without a
// client token. IncorrectState is a rejection of that request, so the attach did
// not apply and re-issuing it does not stack a second attachment; and the EBS
// cache volumes are single-attach, so one already attached answers a resend with
// VolumeInUse rather than a duplicate. The worst case is therefore no worse than
// the pre-existing cold fallback, reached through the caller's detach-and-discard
// cleanup. Every non-IncorrectState error surfaces at once. The retry waits only
// while the instance is coming up — pending, or not yet visible ("") — and fails
// immediately on a terminal instance rather than spinning to the deadline, which
// the caller's bounded context is the backstop for.
func (p *Provider) attachRunningVolume(ctx context.Context, instanceID, volume, device string) error {
	for {
		err := p.api.call(ctx, url.Values{
			"Action": {"AttachVolume"}, "InstanceId": {instanceID},
			"VolumeId": {volume}, "Device": {device},
		}, nil)
		if err == nil {
			return nil
		}
		if code, ok := codeOf(err); !ok || code != "IncorrectState" {
			return err
		}

		// Refused because the instance is not 'running'. Ask what it IS doing — a
		// pending instance (or one not yet visible, "") is worth waiting for; a
		// terminal one is not.
		observed, stateErr := p.describeRaw(ctx, instanceID)
		if stateErr != nil {
			return stateErr
		}
		switch observed.state {
		case "running", "pending", "":
			// Coming up, or the state read lags the attach's own view — wait.
		default:
			return fmt.Errorf("ec2: instance %s is %q, not running, and will not accept "+
				"a cache attachment", instanceID, observed.state)
		}
		if err := p.api.wait(ctx, ebsAttachmentPoll); err != nil {
			return err
		}
	}
}

type cacheVolumeItem struct {
	ID    string `xml:"volumeId"`
	State string `xml:"status"`
	Tags  []struct {
		Key   string `xml:"key"`
		Value string `xml:"value"`
	} `xml:"tagSet>item"`
	Attachments []struct {
		InstanceID string `xml:"instanceId"`
		Device     string `xml:"device"`
		State      string `xml:"status"`
	} `xml:"attachmentSet>item"`
}

func (v cacheVolumeItem) tag(key string) string {
	for _, tag := range v.Tags {
		if tag.Key == key {
			return tag.Value
		}
	}

	return ""
}

func (p *Provider) describeCacheVolumes(
	ctx context.Context,
	values url.Values,
) ([]cacheVolumeItem, error) {
	values.Set("Action", "DescribeVolumes")
	var result struct {
		Volumes []cacheVolumeItem `xml:"volumeSet>item"`
	}
	if err := p.api.call(ctx, values, &result); err != nil {
		return nil, err
	}

	return result.Volumes, nil
}

func (p *Provider) waitCacheVolume(
	ctx context.Context,
	volume string,
	ready func(cacheVolumeItem) bool,
) error {
	for {
		volumes, err := p.describeCacheVolumes(ctx, url.Values{"VolumeId.1": {volume}})
		if err != nil {
			return err
		}
		if len(volumes) != 1 || volumes[0].ID != volume {
			return fmt.Errorf("ec2: cache volume %s disappeared while its attachment changed", volume)
		}
		if ready(volumes[0]) {
			return nil
		}
		if volumes[0].State == "error" || volumes[0].State == "deleted" {
			return fmt.Errorf("ec2: cache volume %s entered %s", volume, volumes[0].State)
		}
		if err := p.api.wait(ctx, ebsAttachmentPoll); err != nil {
			return err
		}
	}
}

func (p *Provider) requireOwnedCacheVolume(ctx context.Context, volume string) error {
	volumes, err := p.describeCacheVolumes(ctx, url.Values{"VolumeId.1": {volume}})
	if err != nil {
		return err
	}
	if len(volumes) != 1 || volumes[0].ID != volume {
		return fmt.Errorf("ec2: cache volume %s does not exist", volume)
	}
	if volumes[0].tag(ownerTag) != p.owner {
		return fmt.Errorf("ec2: cache volume %s belongs to another deployment", volume)
	}

	return nil
}

// DetachVolume uses the volume identity persisted in cache custody. An instance
// termination can remove the attachment before cleanup runs, so rediscovering a
// volume from its former instance and slot would lose the only handle to it.
func (p *Provider) DetachVolume(
	ctx context.Context,
	instanceName string,
	slot int,
	volume string,
) error {
	device, err := attachmentDevice(slot)
	if err != nil {
		return err
	}
	if strings.TrimSpace(instanceName) == "" || !strings.HasPrefix(volume, "vol-") {
		return errors.New("ec2: a cache detachment needs an instance name and an EBS volume id")
	}
	volumes, err := p.describeCacheVolumes(ctx, url.Values{
		"VolumeId.1": {volume},
	})
	if err != nil {
		if code, ok := codeOf(err); ok && code == "InvalidVolume.NotFound" {
			return nil
		}

		return err
	}
	if len(volumes) == 0 {
		return nil
	}
	if len(volumes) != 1 || volumes[0].ID != volume {
		return fmt.Errorf("ec2: volume lookup for %s returned %d entries", volume, len(volumes))
	}
	item := volumes[0]
	if item.tag(ownerTag) != p.owner {
		return fmt.Errorf("ec2: cache volume %s belongs to another deployment", volume)
	}
	if item.State == "available" && len(item.Attachments) == 0 {
		return nil
	}
	if len(item.Attachments) != 1 || item.Attachments[0].Device != device {
		return fmt.Errorf("ec2: cache volume %s is not in slot %d of %s", volume, slot, instanceName)
	}
	instance, found, err := p.Find(ctx, instanceName)
	if err != nil {
		return fmt.Errorf("ec2: resolve cache instance %s before detach: %w", instanceName, err)
	}
	if !found || instance.ID != item.Attachments[0].InstanceID {
		return fmt.Errorf("ec2: cache volume %s is attached to an unexpected instance", volume)
	}
	if err := p.api.call(ctx, url.Values{
		"Action": {"DetachVolume"}, "InstanceId": {instance.ID},
		"VolumeId": {volume}, "Device": {device},
	}, nil); err != nil {
		return fmt.Errorf("ec2: detach cache volume %s from %s: %w", volume, instanceName, err)
	}

	return p.waitCacheVolume(ctx, volume, func(item cacheVolumeItem) bool {
		return item.State == "available" && len(item.Attachments) == 0
	})
}
