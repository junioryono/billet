package images

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerrelease"
)

// ONLY THE PINNED RELEASE HAS A DIGEST BILLET KNOWS, so another release builds
// only with the operator's digest, and a digest that disagrees with the pin is
// refused rather than either one trusted.
func TestTheRunnerDigestComesFromThePinOrTheOperator(t *testing.T) {
	t.Parallel()

	pinnedX64, ok := runnerrelease.PinnedSHA256For("linux-x64")
	if !ok {
		t.Fatal("runnerrelease pins nothing for linux-x64")
	}

	pinnedARM, ok := runnerrelease.PinnedSHA256For("linux-arm64")
	if !ok {
		t.Fatal("runnerrelease pins nothing for linux-arm64")
	}

	supplied := strings.Repeat("c", 64)
	pinned := runnerrelease.Pinned()

	for _, tc := range []struct {
		name     string
		version  string
		arch     string
		supplied string
		want     string
		// refusal is a clause the error must carry; empty means success.
		refusal string
	}{
		{name: "the pinned release on x64", version: pinned, arch: "x64", want: pinnedX64},
		{name: "the pinned release on arm64", version: pinned, arch: "arm64", want: pinnedARM},
		{name: "the pinned release with its own digest", version: pinned, arch: "x64",
			supplied: pinnedX64, want: pinnedX64},
		{name: "the pinned release with another digest", version: pinned, arch: "x64",
			supplied: supplied, refusal: "is not the digest billet pins"},
		// x64's digest named for an arm64 build is the same disagreement.
		{name: "the pinned release with the other architecture's digest", version: pinned,
			arch: "arm64", supplied: pinnedX64, refusal: "is not the digest billet pins"},
		{name: "another release with no digest", version: "2.300.0", arch: "x64",
			refusal: "pass --runner-sha256 with the sha256 of actions-runner-linux-x64-2.300.0.tar.gz"},
		{name: "another release with a digest", version: "2.300.0", arch: "arm64",
			supplied: supplied, want: supplied},
		{name: "a digest that is not one", version: "2.300.0", arch: "x64",
			supplied: strings.Repeat("C", 64), refusal: "is not a sha256"},
		{name: "an architecture billet does not map", version: pinned, arch: "amd64",
			refusal: "is not x64 or arm64"},
		// NO DIGEST MAKES A VERSION A RELEASE, so a malformed one is refused for
		// what it is, with a digest or without, rather than told to bring one.
		{name: "a version with a leading v", version: "v2.337.0", arch: "x64",
			refusal: "is not an actions/runner release version"},
		{name: "a version with a leading v and a digest", version: "v2.337.0", arch: "x64",
			supplied: supplied, refusal: "is not an actions/runner release version"},
		{name: "no version at all", version: "", arch: "x64",
			refusal: "is not an actions/runner release version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := runnerDigest(tc.version, tc.arch, tc.supplied)

			if tc.refusal == "" {
				if err != nil {
					t.Fatalf("runnerDigest(%s, %s, %q): %v", tc.version, tc.arch, tc.supplied, err)
				}

				if got != tc.want {
					t.Errorf("runnerDigest(%s, %s, %q) = %q, want %q",
						tc.version, tc.arch, tc.supplied, got, tc.want)
				}

				return
			}

			if err == nil {
				t.Fatalf("runnerDigest(%s, %s, %q) = %q; it should have refused",
					tc.version, tc.arch, tc.supplied, got)
			}

			if !strings.Contains(err.Error(), tc.refusal) {
				t.Errorf("the refusal %q does not say %q", err, tc.refusal)
			}
		})
	}
}

// AND `billet ami build` ASKS IT, before the config is read and before anything
// is bought. The config path here does not exist, so a build that reached the
// config would refuse for that instead; the runner's refusal arriving first is
// what places the check.
func TestAnUnpinnedRunnerIsRefusedBeforeTheBuildStarts(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent.yaml")

	err := AMI(t.Context(), processEnv(), []string{"build", "--config", missing,
		"--base-image", "ami-base", "--payload-bucket", "bucket", "--runner-version", "2.300.0"})
	if err == nil {
		t.Fatal("a build of an unpinned runner with no digest went ahead")
	}

	if !strings.Contains(err.Error(), "--runner-sha256") {
		t.Errorf("the build refused for another reason: %v", err)
	}

	// THE PINNED RELEASE GETS PAST IT, to the config this test withheld, so the
	// refusal above is the runner rule and not a check that refuses everything.
	err = AMI(t.Context(), processEnv(), []string{"build", "--config", missing,
		"--base-image", "ami-base", "--payload-bucket", "bucket"})
	if err == nil {
		t.Fatal("a build with no config went ahead")
	}

	if !strings.Contains(err.Error(), "no ec2 configuration found in "+missing) {
		t.Errorf("the pinned release did not reach the config this test withheld: %v", err)
	}
}
