package ec2

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// imageOfArch answers DescribeImages with an image AWS says is of arch, refuses
// every RunInstances for capacity, and answers every other call as the fake would.
func imageOfArch(arch string) func(string, url.Values) (int, string) {
	return func(action string, _ url.Values) (int, string) {
		switch action {
		case "RunInstances":
			// EVERY SHAPE REFUSED FOR CAPACITY, so a launch tries all it kept.
			return http.StatusInternalServerError, apiFailure("InsufficientInstanceCapacity")
		case "DescribeImages":
		default:
			return http.StatusOK, defaultReply(action)
		}

		return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
			`<imageId>ami-0abc</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
			`<rootDeviceType>ebs</rootDeviceType><architecture>` + arch + `</architecture>` +
			`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs>` +
			`<deleteOnTermination>true</deleteOnTermination></ebs></item>` +
			`</blockDeviceMapping></item></imagesSet></DescribeImagesResponse>`
	}
}

// mixedShapes declares an x64 shape first, an arm64 one second, and one with no
// architecture last, each large enough for validSpec.
func mixedShapes(cfg *config.EC2Config) {
	cfg.InstanceTypes = []config.EC2InstanceType{
		{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchX64},
		{Type: "c7g.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchARM64},
		{Type: "m7.2xlarge", VCPU: 8, Memory: 32 * config.GiB},
	}
}

// A SHAPE OF ANOTHER ARCHITECTURE THAN THE AMI'S IS NEVER LAUNCHED, first choice or
// fallback (ADR-022): an arm64 AMI on an x64 instance starts and never boots.
func TestALaunchSkipsShapesOfAnotherArchitectureThanItsImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		image string
		want  []string
	}{
		// The x64 shape first in order is skipped, and the shape declaring none is
		// kept as a fallback, as every shape was.
		{image: "arm64", want: []string{"c7g.2xlarge", "m7.2xlarge"}},
		{image: "x86_64", want: []string{"c7i.2xlarge", "m7.2xlarge"}},
		// AN ARCHITECTURE BILLET DOES NOT KNOW narrows nothing.
		{image: "i386", want: []string{"c7i.2xlarge", "c7g.2xlarge", "m7.2xlarge"}},
	} {
		t.Run(tc.image, func(t *testing.T) {
			t.Parallel()

			f := newFakeEC2(t)
			f.respond = imageOfArch(tc.image)
			p := newTestProvider(t, f, mixedShapes)

			if _, err := p.Launch(t.Context(), validSpec()); err == nil {
				t.Fatal("a launch every shape refused succeeded")
			}

			var tried []string
			for _, run := range f.runInstancesCalls() {
				tried = append(tried, run.Get("InstanceType"))
			}

			if strings.Join(tried, ",") != strings.Join(tc.want, ",") {
				t.Errorf("an %s image tried %v, want %v", tc.image, tried, tc.want)
			}
		})
	}
}

// AND WHEN EVERY SHAPE THAT HOLDS IT IS OF ANOTHER ARCHITECTURE, the launch is
// refused before anything is sent, naming the image's architecture and the shapes.
func TestALaunchWithNoShapeOfItsImagesArchitectureIsRefused(t *testing.T) {
	t.Parallel()

	f := newFakeEC2(t)
	f.respond = imageOfArch("arm64")
	p := newTestProvider(t, f, func(cfg *config.EC2Config) {
		cfg.InstanceTypes = []config.EC2InstanceType{
			{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB, Arch: config.ArchX64},
		}
	})

	_, err := p.Launch(t.Context(), validSpec())
	if err == nil || !strings.Contains(err.Error(), "which is arm64, and every declared shape") ||
		!strings.Contains(err.Error(), "c7i.2xlarge (x64)") {
		t.Fatalf("Launch = %v, want a refusal naming the image's architecture and the shapes", err)
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d RunInstances were sent for a shape that cannot boot the image", n)
	}
}
