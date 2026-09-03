package ec2

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// THE BUILDER'S ROOT IS STATED, and nothing here used to state it.
//
// Every build inherited whatever the base image declared — 8GiB on Canonical's
// noble images, most of which GitHub's declared package set occupies. The
// toolcache does not fit in what is left, so provisioning dies on ENOSPC, which
// under `set -e` aborts before the poweroff that signals success. No broken image
// is published; what it costs is a paid builder and a failure that reads as an
// apt problem rather than a disk one.
func TestTheBuilderRootIsSized(t *testing.T) {
	t.Parallel()

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge",
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	}); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	got := f.paramsFor(t, "RunInstances")

	// THE SIZE GOES ON THE ROOT MAPPING, which is index 1 by construction: the
	// root takes 1 and every other device the base image declares follows from 2.
	// Asserting the value alone would pass if it landed on a non-root device,
	// which is a second disk rather than a bigger one.
	if got.Get("BlockDeviceMapping.1.DeviceName") == "" {
		t.Fatal("the root mapping has no device name, so the size below is on nothing")
	}

	size := got.Get("BlockDeviceMapping.1.Ebs.VolumeSize")
	if size == "" {
		t.Fatal("the builder launches with no stated root size, so it inherits the base " +
			"image's — which is the defect this exists to fix")
	}

	gib, err := strconv.Atoi(size)
	if err != nil {
		t.Fatalf("the root size %q is not a number", size)
	}

	if gib != DefaultBuilderDiskGiB {
		t.Errorf("the default build asked for %dGiB, want %d", gib, DefaultBuilderDiskGiB)
	}
}

// AN EXPLICIT SIZE IS THE ONE THAT IS SENT, so the flag is not decoration.
func TestAnExplicitBuilderDiskIsWhatIsRequested(t *testing.T) {
	t.Parallel()

	const want = 64

	b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

	f := newFakeEC2(t)
	f.respond = b.reply

	p := newTestProvider(t, f, nil)

	if _, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge", BuilderDiskGiB: want,
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	}); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if got := f.paramsFor(t, "RunInstances").Get("BlockDeviceMapping.1.Ebs.VolumeSize"); got !=
		strconv.Itoa(want) {
		t.Errorf("asked for %dGiB and the request says %q", want, got)
	}
}

// A SIZE THIS IMAGE CANNOT LIVE IN IS REFUSED BEFORE A BUILDER EXISTS.
//
// A build given too little would launch, provision, and fail the script's own
// free-space check on the instance — true, correct, and paid for. Both directions
// are asserted, because a bound that refuses everything is as useless as none.
func TestABuilderDiskTooSmallForTheImageIsRefusedBeforeAnyLaunch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		gib     int64
		wantErr bool
	}{
		{name: "negative", gib: -1, wantErr: true},
		{name: "one GiB", gib: 1, wantErr: true},
		{name: "one under the minimum", gib: minBuilderDiskGiB - 1, wantErr: true},
		{name: "exactly the minimum", gib: minBuilderDiskGiB},
		{name: "the default", gib: DefaultBuilderDiskGiB},
		// ZERO IS NOT "TOO SMALL", it is "unset", and a bound that refused it
		// would refuse every build that does not pass the flag.
		{name: "unset", gib: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

			f := newFakeEC2(t)
			f.respond = b.reply

			p := newTestProvider(t, f, nil)

			_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
				BaseImage: "ami-base", InstanceType: "c7i.xlarge", BuilderDiskGiB: tc.gib,
				Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
			})

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a %dGiB builder root was refused: %v", tc.gib, err)
				}

				return
			}

			if err == nil {
				t.Fatalf("a %dGiB builder root was accepted", tc.gib)
			}

			// AND NOTHING WAS LAUNCHED. The error alone does not say that, and a
			// refusal raised after RunInstances has already been paid for.
			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("%d builders were launched for a %dGiB root", n, tc.gib)
			}
		})
	}
}

// THE SCRIPT LOOKS BEFORE IT INSTALLS, because growing an EBS volume does not
// grow the filesystem on it.
//
// cloud-init's growpart runs in the `init` stage and user data in `final`, so it
// should already have happened — but that is an ordering somebody else owns, and
// the failure if it did not is dpkg complaining about disk space with nothing
// naming the volume.
func TestTheScriptRefusesToProvisionOntoADiskThatDidNotGrow(t *testing.T) {
	t.Parallel()

	script := mustScript(t)

	lines := strings.Split(script, "\n")

	check := lineOf(t, lines, "billet_free_kib=$(df -Pk /")
	apt := firstLineOf(t, lines, "apt-get -o DPkg::Lock::Timeout=600 update")

	if check >= apt {
		t.Errorf("the free-space check is at line %d and the first apt transaction at %d; "+
			"checking after installing is checking after the failure", check, apt)
	}

	// THE THRESHOLD IS THE CONSTANT, not a number typed twice. A script asserting
	// a different figure from the one the flag is validated against would let a
	// build pass validation and fail on the instance.
	if !strings.Contains(script, strconv.Itoa(minBuilderFreeGiB*1024*1024)) {
		t.Errorf("the script does not check for %dGiB in KiB, so its threshold and "+
			"minBuilderFreeGiB have drifted", minBuilderFreeGiB)
	}

	// df -P, BECAUSE THE DEFAULT FORMAT WRAPS. A long device name goes onto its
	// own line and `NR == 2` then reads the wrong record — reporting a free space
	// that belongs to no filesystem.
	if !strings.Contains(script, "df -Pk /") {
		t.Error("the free-space check does not use df -P, so a long device name would wrap " +
			"and the awk record would be the wrong one")
	}
}

// A BUILDER ROOT UNDER THE BASE IMAGE'S OWN IS REFUSED, and nothing tested that.
//
// TestABuilderDiskTooSmallForTheImageIsRefusedBeforeAnyLaunch exercises only
// minBuilderDiskGiB: its base image omits <volumeSize>, so rootGiB stays zero and
// the snapshot floor never fires. Deleting that check entirely would have left it
// green — a review caught it.
//
// EBS will not create a volume smaller than the snapshot behind it, so such a
// launch is refused by AWS with a parameter error naming neither the base image
// nor the number. billet says both instead.
func TestABuilderDiskUnderTheBaseImagesRootIsRefused(t *testing.T) {
	t.Parallel()

	const baseGiB = 64

	for _, tc := range []struct {
		name    string
		ask     int64
		wantErr bool
	}{
		// AN UNSET SIZE ADAPTS. Somebody whose base AMI declares a large root and
		// who passes no flag had a working build before this; refusing it would
		// have told them to pass at least `--builder-disk 64` for a flag they
		// never used, about a number the default chose for them.
		{name: "the default, under a large base image", ask: 0},
		// AN EXPLICIT ONE IS REFUSED, because they typed it and are present to
		// read the answer.
		{name: "an explicit size under the base image", ask: baseGiB - 1, wantErr: true},
		{name: "exactly the base image's root", ask: baseGiB},
		{name: "larger than the base image's root", ask: baseGiB + 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newFakeEC2(t)
			f.respond = builderImageWithRoot(t, baseGiB)

			p := newTestProvider(t, f, nil)

			_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
				BaseImage: "ami-base", InstanceType: "c7i.xlarge", BuilderDiskGiB: tc.ask,
				Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
			})

			if !tc.wantErr {
				// The build proceeds past the floor; whether it completes depends
				// on the rest of the fake, which is not this test's subject.
				if err != nil && strings.Contains(err.Error(), "smaller than its snapshot") {
					t.Fatalf("a %dGiB root against a %dGiB base was refused by the floor: %v",
						tc.ask, baseGiB, err)
				}

				return
			}

			if err == nil {
				t.Fatalf("a %dGiB root against a %dGiB base image was accepted; EBS refuses "+
					"a volume smaller than its snapshot", tc.ask, baseGiB)
			}

			// THE ERROR NAMES BOTH THE NUMBER AND THE IMAGE, which is the whole
			// reason billet checks rather than letting AWS answer.
			for _, want := range []string{strconv.Itoa(baseGiB), "ami-base"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not mention %q: %v", want, err)
				}
			}

			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("%d builders were launched for a root under the base image's", n)
			}
		})
	}
}

// AND IT STILL FIRES WHEN THE LAYOUT CAME FROM THE CACHE, since imageLayout
// memoises per image and a check reading only a fresh lookup would pass the
// second time.
func TestTheBaseImageFloorSurvivesTheLayoutCache(t *testing.T) {
	t.Parallel()

	const baseGiB = 64

	f := newFakeEC2(t)
	f.respond = builderImageWithRoot(t, baseGiB)

	p := newTestProvider(t, f, nil)

	spec := BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		BaseImage: "ami-base", InstanceType: "c7i.xlarge", BuilderDiskGiB: baseGiB - 1,
		Arch: "x64", RunnerVersion: "2.328.0", Name: "test-image",
	}

	for i := range 2 {
		if _, err := p.BuildImage(t.Context(), spec); err == nil {
			t.Fatalf("attempt %d was accepted; the floor must not depend on the layout "+
				"being freshly fetched", i+1)
		}
	}

	if n := f.countOf("RunInstances"); n != 0 {
		t.Errorf("%d builders were launched across two refused attempts", n)
	}
}

// builderImageWithRoot answers the build path's lookups with a base image whose
// root declares a size. The shared fixture omits it deliberately.
func builderImageWithRoot(t *testing.T, gib int) func(string, url.Values) (int, string) {
	t.Helper()

	return func(action string, _ url.Values) (int, string) {
		if action == "DescribeImages" {
			return http.StatusOK, `<DescribeImagesResponse><imagesSet><item>` +
				`<imageId>ami-base</imageId><rootDeviceName>/dev/xvda</rootDeviceName>` +
				`<rootDeviceType>ebs</rootDeviceType>` +
				`<blockDeviceMapping><item><deviceName>/dev/xvda</deviceName><ebs>` +
				`<deleteOnTermination>true</deleteOnTermination>` +
				`<volumeSize>` + strconv.Itoa(gib) + `</volumeSize>` +
				`</ebs></item></blockDeviceMapping>` +
				`</item></imagesSet></DescribeImagesResponse>`
		}

		return 0, ""
	}
}

// firstLineOf is lineOf for a marker that legitimately recurs.
//
// lineOf REFUSES AN AMBIGUOUS MARKER, which is right and is how this was caught:
// the provisioning script runs its own apt transaction AND now carries the shared
// installers, which run theirs. What an ordering assertion about the script means
// is the FIRST one — the transaction that installs the packages everything else
// depends on. Saying which is meant beats loosening lineOf for every caller.
func firstLineOf(t *testing.T, lines []string, marker string) int {
	t.Helper()

	for i, l := range lines {
		if strings.Contains(l, marker) {
			return i
		}
	}

	t.Fatalf("the script never does %q", marker)

	return -1
}
