package ec2

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

func TestEC2HotAttachesAndDetachesAnOwnedCacheVolume(t *testing.T) {
	f := newFakeEC2(t)
	attached := false
	f.respond = func(action string, params url.Values) (int, string) {
		switch action {
		case "DescribeInstances":
			return http.StatusOK, describeReply("", instanceXML("i-123", "billet-lease-1"))
		case "AttachVolume":
			attached = true

			return http.StatusOK, `<AttachVolumeResponse><status>attaching</status></AttachVolumeResponse>`
		case "DescribeVolumes":
			if params.Get("VolumeId.1") != "" {
				state := "available"
				attachment := ""
				if attached {
					state = "in-use"
					attachment = `<attachmentSet><item><instanceId>i-123</instanceId><device>/dev/sdh</device><status>attached</status></item></attachmentSet>`
				}

				return http.StatusOK, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-123</volumeId><status>` + state + `</status><tagSet><item><key>sh.billet.owner</key><value>dep-1</value></item></tagSet>` + attachment + `</item></volumeSet></DescribeVolumesResponse>`
			}

			return http.StatusOK, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-123</volumeId><status>in-use</status><tagSet><item><key>sh.billet.owner</key><value>dep-1</value></item></tagSet><attachmentSet><item><instanceId>i-123</instanceId><device>/dev/sdh</device><status>attached</status></item></attachmentSet></item></volumeSet></DescribeVolumesResponse>`
		case "DetachVolume":
			attached = false

			return http.StatusOK, `<DetachVolumeResponse><status>detaching</status></DetachVolumeResponse>`
		default:
			return http.StatusOK, defaultReply(action)
		}
	}
	p := newTestProvider(t, f, nil)
	p.api.sleep = func(_ context.Context, _ time.Duration) error { return nil }

	if err := p.AttachVolume(t.Context(), "billet-lease-1", 2, "vol-123"); err != nil {
		t.Fatalf("AttachVolume: %v", err)
	}
	attach := f.paramsFor(t, "AttachVolume")
	if attach.Get("InstanceId") != "i-123" || attach.Get("VolumeId") != "vol-123" ||
		attach.Get("Device") != "/dev/sdh" {
		t.Fatalf("AttachVolume params = %v", attach)
	}
	if err := p.DetachVolume(t.Context(), "billet-lease-1", 2, "vol-123"); err != nil {
		t.Fatalf("DetachVolume: %v", err)
	}
	detach := f.paramsFor(t, "DetachVolume")
	if detach.Get("InstanceId") != "i-123" || detach.Get("VolumeId") != "vol-123" ||
		detach.Get("Device") != "/dev/sdh" {
		t.Fatalf("DetachVolume params = %v", detach)
	}
}

func TestEC2RefusesToDetachAnotherDeploymentsCacheVolume(t *testing.T) {
	f := newFakeEC2(t)
	f.respond = func(action string, _ url.Values) (int, string) {
		if action == "DescribeVolumes" {
			return http.StatusOK, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-foreign</volumeId><status>available</status><tagSet><item><key>sh.billet.owner</key><value>another-deployment</value></item></tagSet></item></volumeSet></DescribeVolumesResponse>`
		}

		return http.StatusOK, defaultReply(action)
	}
	p := newTestProvider(t, f, nil)

	if err := p.DetachVolume(t.Context(), "billet-lease-1", 2, "vol-foreign"); err == nil {
		t.Fatal("DetachVolume accepted another deployment's cache volume")
	}
	if got := f.countOf("DetachVolume"); got != 0 {
		t.Fatalf("DetachVolume API calls = %d, want 0", got)
	}
}

func TestEC2CacheDeviceUsesTheEBSVolumeIdentity(t *testing.T) {
	p := newTestProvider(t, newFakeEC2(t), nil)
	if got := p.GuestVolumeDevice(0, "vol-0123abcd"); got !=
		"/dev/disk/by-id/nvme-Amazon_Elastic_Block_Store_vol0123abcd" {
		t.Fatalf("guest cache device = %q", got)
	}
	var _ provider.VolumeAttacher = p
}
