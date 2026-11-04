package images

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/provider/ec2"
	"github.com/junioryono/billet/internal/runnerrelease"
)

// A WINDOWS IMAGE'S RUNNER IS CHECKED AGAINST THE WINDOWS PIN, under the archive's own
// name, and never against an architecture's Linux tarball.
func TestAWindowsBuildChecksItsRunnerAgainstTheWindowsPin(t *testing.T) {
	t.Parallel()

	pinned, ok := ec2.PinnedWindowsRunnerSHA256()
	if !ok {
		t.Fatal("no win-x64 digest is pinned")
	}

	got, err := runnerDigestForOS(true, runnerrelease.Pinned(), "x64", "")
	if err != nil || got != pinned {
		t.Fatalf("the pinned release answered %q, %v; want %q", got, err, pinned)
	}

	linux, _ := ec2.PinnedRunnerSHA256("x64")
	if got == linux {
		t.Fatal("the Windows pin is Linux's digest")
	}

	// LINUX'S DIGEST FOR THE PINNED RELEASE IS TWO ANSWERS THAT DIFFER.
	if _, err := runnerDigestForOS(true, runnerrelease.Pinned(), "x64", linux); err == nil ||
		!strings.Contains(err.Error(), "actions-runner-win-x64-") {
		t.Fatalf("a digest that is not the Windows pin was answered %v", err)
	}

	if _, err := runnerDigestForOS(true, "2.0.0", "x64", ""); err == nil ||
		!strings.Contains(err.Error(), ".zip") {
		t.Fatalf("an unpinned release with no digest was answered %v", err)
	}

	unpinned := strings.Repeat("a", 64)
	if got, err := runnerDigestForOS(true, "2.0.0", "x64", unpinned); err != nil || got != unpinned {
		t.Fatalf("an unpinned release with its digest answered %q, %v", got, err)
	}

	if got, err := runnerDigestForOS(false, runnerrelease.Pinned(), "x64", ""); err != nil || got != linux {
		t.Fatalf("a Linux build answered %q, %v; want %q", got, err, linux)
	}
}

// WHAT A WINDOWS BUILD CANNOT DO IS REFUSED BEFORE THE CONFIG IS READ OR ANYTHING BOUGHT.
func TestAMIBuildRefusesWhatAWindowsImageCannotCarry(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"an unknown os": {[]string{"--os", "plan9"}, "is not linux or windows"},
		"arm64":         {[]string{"--os", "windows", "--arch", "arm64"}, "x64 alone"},
		"a cache CA":    {[]string{"--os", "windows", "--ca-cert", "ca.pem"}, "no client for billet's caches"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out bytes.Buffer

			args := append([]string{"build", "--base-image", "ami-0abc", "--payload-bucket", "bucket"}, tc.args...)

			err := AMI(context.Background(), cli.Env{Stdout: &out, Stderr: &out, Getenv: os.Getenv}, args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AMI answered %v, want %q", err, tc.want)
			}
		})
	}
}
