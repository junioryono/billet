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

const ebsAttachmentPoll = 2 * time.Second

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
	if err := p.api.call(ctx, url.Values{
		"Action": {"AttachVolume"}, "InstanceId": {instance.ID},
		"VolumeId": {volume}, "Device": {device},
	}, nil); err != nil {
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
