package ebss3

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// volumeAttachment is one volume's attachment as EC2 describes it.
type volumeAttachment struct {
	// Instance is the instance it is attached or attaching to, empty when it is attached
	// to none.
	Instance string
	// State is the attachment's: attaching, attached, detaching or busy.
	State string
	// Attached is when the attachment began.
	Attached time.Time
}

type describedVolume struct {
	ID          string        `xml:"volumeId"`
	State       string        `xml:"status"`
	Tags        []resourceTag `xml:"tagSet>item"`
	Attachments []struct {
		Instance string    `xml:"instanceId"`
		State    string    `xml:"status"`
		Attached time.Time `xml:"attachTime"`
	} `xml:"attachmentSet>item"`
}

// describeVolume answers one volume, found=false when EC2 says it does not exist.
func (e ebsAPI) describeVolume(ctx context.Context, id string) (describedVolume, bool, error) {
	var result struct {
		Volumes []describedVolume `xml:"volumeSet>item"`
	}
	err := e.call(ctx, url.Values{"Action": {"DescribeVolumes"}, "VolumeId.1": {id}}, &result)
	if awsCode(err) == "InvalidVolume.NotFound" {
		return describedVolume{}, false, nil
	}
	if err != nil {
		return describedVolume{}, false, err
	}
	if len(result.Volumes) != 1 || result.Volumes[0].ID != id {
		return describedVolume{}, false, fmt.Errorf("ebs-s3: volume %s was not uniquely described", id)
	}

	return result.Volumes[0], true, nil
}

// Attachment answers where a volume is attached. found=false means EC2 says the volume
// does not exist; an attachment with no instance means it is attached to none.
func (e ebsAPI) Attachment(ctx context.Context, id string) (volumeAttachment, bool, error) {
	volume, found, err := e.describeVolume(ctx, id)
	if err != nil || !found {
		return volumeAttachment{}, found, err
	}
	if len(volume.Attachments) > 1 {
		return volumeAttachment{}, true, fmt.Errorf("ebs-s3: volume %s has %d attachments, which a "+
			"cache volume never has", id, len(volume.Attachments))
	}
	if len(volume.Attachments) == 0 {
		return volumeAttachment{}, true, nil
	}

	a := volume.Attachments[0]

	return volumeAttachment{Instance: a.Instance, State: a.State, Attached: a.Attached}, true, nil
}

// AttachVolume attaches one of this store's volumes to instance at point, a device
// name the instance does not use. Only a volume carrying both of this store's ownership
// tags is attached, as only such a one is deleted.
func (e ebsAPI) AttachVolume(ctx context.Context, id, instance, point string) error {
	owned, found, err := e.volumeOwned(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("ebs-s3: volume %s does not exist to attach", id)
	}
	if !owned {
		return fmt.Errorf("ebs-s3: refusing to attach volume %s: %w", id, errNotOwned)
	}

	return e.call(ctx, url.Values{
		"Action": {"AttachVolume"}, "VolumeId": {id}, "InstanceId": {instance}, "Device": {point},
	}, nil)
}

// DetachVolume detaches one of this store's volumes from instance. It does not force:
// a forced detach is EC2's last resort against an unresponsive instance and can lose
// data, and the caller has already proved nothing on the host holds the volume.
func (e ebsAPI) DetachVolume(ctx context.Context, id, instance string) error {
	owned, found, err := e.volumeOwned(ctx, id)
	if err != nil || !found {
		return err
	}
	if !owned {
		return fmt.Errorf("ebs-s3: refusing to detach volume %s: %w", id, errNotOwned)
	}

	err = e.call(ctx, url.Values{"Action": {"DetachVolume"}, "VolumeId": {id}, "InstanceId": {instance}}, nil)
	switch awsCode(err) {
	case "InvalidVolume.NotFound", "IncorrectState":
		// GONE, OR ALREADY DETACHED OR DETACHING: what the caller waits for next is the
		// volume's own state, which says which.
		return nil
	}

	return err
}

// AwaitDetached waits until EC2 describes the volume as available, as every create and
// clone does.
func (e ebsAPI) AwaitDetached(ctx context.Context, id string) error {
	return e.waitVolume(ctx, id, "available")
}

// AttachedTo lists this store's volumes attached or attaching to instance, with when
// each attachment began.
func (e ebsAPI) AttachedTo(ctx context.Context, instance string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	seen := map[string]bool{}
	for next := ""; ; {
		values := url.Values{
			"Action":           {"DescribeVolumes"},
			"Filter.1.Name":    {"attachment.instance-id"},
			"Filter.1.Value.1": {instance},
			"Filter.2.Name":    {"tag:" + cacheOwnerTag},
			"Filter.2.Value.1": {e.owner},
		}
		if next != "" {
			values.Set("NextToken", next)
		}
		var result struct {
			Volumes   []describedVolume `xml:"volumeSet>item"`
			NextToken string            `xml:"nextToken"`
		}
		if err := e.call(ctx, values, &result); err != nil {
			return nil, err
		}
		for _, volume := range result.Volumes {
			if !strings.HasPrefix(volume.ID, "vol-") || !e.owned(volume.Tags) {
				continue
			}
			for _, a := range volume.Attachments {
				if a.Instance == instance {
					out[volume.ID] = a.Attached
				}
			}
		}
		if result.NextToken == "" {
			return out, nil
		}
		if seen[result.NextToken] {
			return nil, errors.New("ebs-s3: a paginated EBS volume listing repeated its token")
		}
		seen[result.NextToken] = true
		next = result.NextToken
	}
}
