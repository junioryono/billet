package ec2

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

func ownedCacheVolumeReply(attached bool) string {
	state := "available"
	attachment := ""
	if attached {
		state = "in-use"
		attachment = `<attachmentSet><item><instanceId>i-123</instanceId>` +
			`<device>/dev/sdh</device><status>attached</status></item></attachmentSet>`
	}

	return `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-123</volumeId><status>` +
		state + `</status><tagSet><item><key>sh.billet.owner</key><value>dep-1</value></item>` +
		`</tagSet>` + attachment + `</item></volumeSet></DescribeVolumesResponse>`
}

// TestEC2AttachRetriesThroughThePendingToRunningRace proves an attach that loses
// the boot race is retried rather than abandoned cold. EC2 can still report an
// instance not 'running' — AttachVolume answering IncorrectState — for a short
// window after the guest has booted and its runner is already asking for cache;
// falling back cold there would leave a producer job's warm image store
// unwritten and break the consumer's --pull=never. The instance is 'pending'
// across the refusals, so the wait-on-pending branch is exercised, not just a
// same-state retry.
func TestEC2AttachRetriesThroughThePendingToRunningRace(t *testing.T) {
	f := newFakeEC2(t)
	attachAttempts := 0
	attached := false
	instanceStates := []string{"pending", "running"}
	describeRaws := 0
	f.respond = func(action string, params url.Values) (int, string) {
		switch action {
		case "DescribeInstances":
			// Find (no InstanceId filter) resolves the instance; describeRaw
			// (InstanceId.1 set) walks pending -> running across the refusals.
			state := "running"
			if params.Get("InstanceId.1") != "" {
				state = instanceStates[min(describeRaws, len(instanceStates)-1)]
				describeRaws++
			}

			return http.StatusOK, describeReply("", instanceXMLInState("i-123", "billet-lease-1", state))
		case "AttachVolume":
			attachAttempts++
			if attachAttempts < 3 {
				return http.StatusBadRequest, apiFailure("IncorrectState")
			}
			attached = true

			return http.StatusOK, `<AttachVolumeResponse><status>attaching</status></AttachVolumeResponse>`
		case "DescribeVolumes":
			return http.StatusOK, ownedCacheVolumeReply(attached)
		default:
			return http.StatusOK, defaultReply(action)
		}
	}
	p := newTestProvider(t, f, nil)
	p.api.sleep = func(_ context.Context, _ time.Duration) error { return nil }

	if err := p.AttachVolume(t.Context(), "billet-lease-1", 2, "vol-123"); err != nil {
		t.Fatalf("AttachVolume across a pending->running race: %v", err)
	}
	if attachAttempts != 3 {
		t.Fatalf("AttachVolume attempts = %d, want 3 — it must retry through IncorrectState, "+
			"not fall back cold on the first refusal", attachAttempts)
	}
	if describeRaws < 2 {
		t.Fatalf("instance state was read %d times; the pending->running wait was not exercised", describeRaws)
	}
}

// TestEC2AttachFailsFastWhenTheInstanceWillNotRun proves the retry is bounded by
// what the instance is actually doing: a terminal instance never becomes
// running, so the attach reports at once instead of spinning on IncorrectState
// until the deadline. The sleep seam fails the test if reached, so a regression
// that dropped the terminal bail fails promptly instead of hammering the API.
func TestEC2AttachFailsFastWhenTheInstanceWillNotRun(t *testing.T) {
	f := newFakeEC2(t)
	attachAttempts := 0
	f.respond = func(action string, _ url.Values) (int, string) {
		switch action {
		case "DescribeInstances":
			return http.StatusOK, describeReply("", instanceXMLInState("i-123", "billet-lease-1", "stopped"))
		case "AttachVolume":
			attachAttempts++

			return http.StatusBadRequest, apiFailure("IncorrectState")
		case "DescribeVolumes":
			return http.StatusOK, ownedCacheVolumeReply(false)
		default:
			return http.StatusOK, defaultReply(action)
		}
	}
	p := newTestProvider(t, f, nil)
	p.api.sleep = func(_ context.Context, _ time.Duration) error {
		t.Error("attach slept before retrying a terminal instance; it must fail fast")

		return context.Canceled
	}

	err := p.AttachVolume(t.Context(), "billet-lease-1", 2, "vol-123")
	if err == nil {
		t.Fatal("AttachVolume accepted an attach to an instance that will never run")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("error = %v; want it to name the not-running instance", err)
	}
	if attachAttempts != 1 {
		t.Fatalf("AttachVolume attempts = %d, want 1 — a terminal instance must fail fast, "+
			"not retry until the deadline", attachAttempts)
	}
}

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
