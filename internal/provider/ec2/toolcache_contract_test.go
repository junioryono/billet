package ec2

import (
	"strconv"
	"strings"
	"testing"
)

// THE STAMP SAYS WHAT THE IMAGE ACTUALLY CARRIES, per architecture.
//
// The tag is what `billet check` reads INSTEAD of looking at the image, so a
// stamp the build does not honour is worse than no stamp: it is a claim nothing
// verifies. What is asserted here is the AGREEMENT between the two, not a
// hardcoded answer per architecture — arm64 carried no toolcache until the
// installers learned every vendor's arch spelling, and a test written as
// `arm64 -> 1` had to be edited to accept the fix rather than confirming it.
//
// BOTH DIRECTIONS, because a contractFor returning 1 for everything would also
// "not lie" and would silently stop any image from claiming the toolcache.
func TestTheStampedContractMatchesWhatTheArchitectureCarries(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		arch          string
		want          int
		wantToolcache bool
	}{
		{arch: "x64", want: AMIContract, wantToolcache: true},
		// arm64 CARRIES THE TOOLCACHE NOW. Every vendor's arch spelling comes from
		// one variable — node and Python use the tool-cache name, go and temurin
		// use dpkg's, pypy calls it aarch64, ruby-builder suffixes arm64 and leaves
		// x64 bare — so the same six tools install under `<version>/arm64`.
		{arch: "arm64", want: AMIContract, wantToolcache: true},
	} {
		t.Run(tc.arch, func(t *testing.T) {
			t.Parallel()

			if got := contractFor(tc.arch); got != tc.want {
				t.Errorf("a %s image is stamped contract %d, want %d", tc.arch, got, tc.want)
			}

			script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: "2.328.0", Arch: tc.arch})
			if err != nil {
				t.Fatalf("provisionScript for %s: %v", tc.arch, err)
			}

			has := strings.Contains(script, "billet_install_toolcache")
			if has != tc.wantToolcache {
				t.Errorf("a %s build installs a toolcache=%v, want %v; the stamp and the "+
					"script have to agree or the tag is a claim about nothing",
					tc.arch, has, tc.wantToolcache)
			}

			// AND THE SCRIPT BUILDS FOR THE ARCHITECTURE IT WAS ASKED FOR. The two
			// assertions above would both pass against a build that installed an
			// x64 toolcache onto an arm64 image: the stamp would agree with the
			// script, and both would be describing the wrong machine.
			if !tc.wantToolcache {
				return
			}

			if want := "BILLET_TC_ARCH=" + tc.arch; !strings.Contains(script, want) {
				t.Errorf("a %s build does not pass %q to the installers; an x64 toolcache "+
					"on an arm64 image is structurally complete and every binary is the "+
					"wrong format", tc.arch, want)
			}

			if want := "/" + tc.arch + ".complete"; !strings.Contains(script, want) {
				t.Errorf("a %s build's gate does not look for %q, which is the marker "+
					"@actions/tool-cache stats on that architecture", tc.arch, want)
			}
		})
	}

	// AND THE CONSTANT MOVED, so an image built before this carries a lower number
	// and `billet check` can tell. A contract that never goes up cannot express
	// that an image is missing something newly required.
	if AMIContract < 2 {
		t.Errorf("AMIContract is %d; the toolcache is a newly required property and an image "+
			"without it must be distinguishable from one with it", AMIContract)
	}
}

// THE PROMOTION WRITES THE CONTRACT IT WAS ASKED FOR, and this reads the request
// rather than the constant.
//
// stampImage used to write the package-level AMIContract unconditionally, so
// every image claimed whatever the binary claimed. Passing it in is what lets an
// architecture be honest, and a test that asserted the constant would pass either
// way. The stamp has since moved off CreateImage entirely — it is written after
// the image has been booted and proved — but the property is the same one.
func TestTheImageIsTaggedWithTheContractItWasProvedAt(t *testing.T) {
	t.Parallel()

	for _, contract := range []int{1, AMIContract} {
		b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

		f := newFakeEC2(t)
		f.respond = b.reply

		p := newTestProvider(t, f, nil)

		if err := p.promoteContract(t.Context(), "ami-new", contract); err != nil {
			t.Fatalf("promoteContract at contract %d: %v", contract, err)
		}

		got := f.paramsFor(t, "CreateTags")

		if got.Get("Tag.1.Key") != amiContractTag {
			t.Fatalf("the contract is not tag 1; the assertion below reads the wrong tag")
		}

		if v := got.Get("Tag.1.Value"); v != strconv.Itoa(contract) {
			t.Errorf("a promotion at contract %d stamped %q", contract, v)
		}
	}
}
