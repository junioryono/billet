package ec2

import (
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// imageWithRoot answers DescribeImages with an image whose root declares a size,
// which the shared fixture deliberately does not.
//
// The shared one omits volumeSize, so rootGiB stays zero there and neither the
// floor nor the clamp can fire — which is right for tests about other things and
// useless for these.
func imageWithRoot(t *testing.T, gib int) func(string, url.Values) (int, string) {
	t.Helper()

	return func(action string, _ url.Values) (int, string) {
		switch action {
		case "DescribeImages":
			return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
				`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
				`<rootDeviceType>ebs</rootDeviceType>` +
				`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs>` +
				`<deleteOnTermination>true</deleteOnTermination>` +
				`<volumeSize>` + strconv.Itoa(gib) + `</volumeSize>` +
				`</ebs></item></blockDeviceMapping>` +
				`</item></imagesSet></DescribeImagesResponse>`

		case "RunInstances":
			return http.StatusOK, `<RunInstancesResponse><instancesSet><item>` +
				`<instanceId>i-0abc</instanceId>` +
				`<instanceState><name>pending</name></instanceState>` +
				`</item></instancesSet></RunInstancesResponse>`
		}

		return 0, ""
	}
}

// A TIER ASKING FOR LESS DISK THAN ITS IMAGE DECLARES GETS THE IMAGE'S ROOT.
//
// EBS will not create a volume smaller than the snapshot behind it, so such a
// launch is refused at RunInstances with a parameter error naming neither the
// tier nor the image. That was always true and was invisible while runner images
// were small; sizing the builder's root makes an ordinary `disk:` land under it.
//
// CLAMPED UP RATHER THAN REFUSED because `Disk` is what a job needs AT LEAST and
// disk is not capacity the allocator accounts for — so more costs pennies and
// satisfies the request, while refusing fails somebody's CI over a number they
// did not choose.
func TestATierAskingForLessDiskThanTheImageGetsTheImagesRoot(t *testing.T) {
	t.Parallel()

	const imageGiB = 30

	f := newFakeEC2(t)
	f.respond = imageWithRoot(t, imageGiB)

	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 20 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize")
	if got != strconv.Itoa(imageGiB) {
		t.Errorf("a tier asking for 20GiB against a %dGiB image launched with %q; EBS refuses "+
			"anything under the snapshot, so this is a failed job rather than a small disk",
			imageGiB, got)
	}
}

// AND A TIER ASKING FOR MORE STILL GETS MORE, which the clamp must not eat.
//
// A max() written the wrong way round, or a clamp that replaced rather than
// raised, would silently shrink every tier that asked for a real disk — and the
// test above would still pass, because it only ever looks at the small case.
func TestATierAskingForMoreDiskThanTheImageKeepsIt(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)
	f.respond = imageWithRoot(t, 30)

	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 100 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got !=
		"100" {
		t.Errorf("a tier asking for 100GiB launched with %q", got)
	}
}

// A TIER THAT ASKS FOR NO DISK STATES NO SIZE, which is not the same as asking
// for the image's.
//
// Stating the image's own size would be harmless and wrong: it turns an absent
// preference into an explicit request, so a later image change silently stops
// being what the tier launches with.
func TestATierWithNoDiskStatesNoSize(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)
	f.respond = imageWithRoot(t, 30)

	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 0

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got != "" {
		t.Errorf("a tier stating no disk launched with an explicit %q", got)
	}
}

// AN IMAGE THAT REPORTS NO SIZE LEAVES THE TIER'S NUMBER ALONE.
//
// rootGiB is zero when the image did not say or billet could not parse it, and
// zero must read as "no floor known" rather than as a floor of zero — the latter
// is the same code path and would be invisible.
func TestAnImageThatReportsNoRootSizeDoesNotClamp(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)

	p := newTestProvider(t, f, nil)

	spec := validSpec()
	spec.Disk = 20 * config.GiB

	if _, err := p.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got !=
		"20" {
		t.Errorf("an image reporting no root size clamped a 20GiB tier to %q", got)
	}
}

// THE FLOOR IS READ FROM THE ROOT DEVICE, not from whichever mapping came last.
//
// DescribeImages promises no order, and a non-root device's size is not a floor
// for the root — reading one would clamp every tier to the size of a data disk
// that has nothing to do with the root volume.
//
// BOTH ORDERS, because one of them cannot fail. The first version of this put the
// root LAST, so a bug that parsed every device still ended on the root's value
// and the mutant survived: it was testing the order of the fixture rather than
// the predicate. With the root FIRST, a parse-everything bug ends on the data
// device and the clamp jumps to its size.
func TestTheRootSizeIsReadFromTheRootDevice(t *testing.T) {
	t.Parallel()

	root := `<item><deviceName>/dev/xvda</deviceName><ebs>` +
		`<deleteOnTermination>true</deleteOnTermination>` +
		`<volumeSize>8</volumeSize></ebs></item>`
	data := `<item><deviceName>/dev/sdb</deviceName><ebs>` +
		`<deleteOnTermination>true</deleteOnTermination>` +
		`<volumeSize>500</volumeSize></ebs></item>`

	for _, tc := range []struct {
		name     string
		mappings string
	}{
		{name: "the root is listed first", mappings: root + data},
		{name: "the root is listed last", mappings: data + root},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeEC2(t)
			f.respond = func(action string, _ url.Values) (int, string) {
				switch action {
				case "DescribeImages":
					return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
						`<imageId>ami-0abc</imageId>` +
						`<rootDeviceName>/dev/xvda</rootDeviceName>` +
						`<rootDeviceType>ebs</rootDeviceType><blockDeviceMapping>` +
						tc.mappings +
						`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`

				case "RunInstances":
					return http.StatusOK, `<RunInstancesResponse><instancesSet><item>` +
						`<instanceId>i-0abc</instanceId>` +
						`<instanceState><name>pending</name></instanceState>` +
						`</item></instancesSet></RunInstancesResponse>`
				}

				return 0, ""
			}

			p := newTestProvider(t, f, nil)

			spec := validSpec()
			spec.Disk = 20 * config.GiB

			if _, err := p.Launch(t.Context(), spec); err != nil {
				t.Fatalf("Launch: %v", err)
			}

			got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize")
			if got != "20" {
				t.Errorf("the tier asked for 20GiB against an 8GiB root and launched with %q; "+
					"500 here means the size came from the data device", got)
			}
		})
	}
}
