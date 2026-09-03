package ec2

import (
	"strings"
	"testing"
)

// A RUNNER VERSION IS INTERPOLATED INTO A ROOT SHELL, so it is validated before
// a builder exists.
//
// `strconv.Quote` was standing in for shell quoting and is not: it produces a GO
// double-quoted literal, and shell double quotes do not suppress command
// substitution. Measured rather than reasoned about — the exact construction with
// `$(id -u)` resolved to the uid before curl ever saw the URL.
//
// The payload that makes this worse than ordinary injection is `poweroff` itself.
// It is billet's SUCCESS SIGNAL: a self-stopped builder is what tells the build
// the provisioning finished, so an injected shutdown does not merely run a
// command — it publishes a half-provisioned AMI as though every step had passed.
func TestARunnerVersionThatIsNotAReleaseIsRefusedBeforeAnyLaunch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		version string
	}{
		{"command substitution", "$(poweroff)"},
		{"command substitution inside a version", "2.328.0$(poweroff)"},
		{"backticks", "2.328.0`poweroff`"},
		{"a closing quote and a second command", `2.328.0"; poweroff; echo "`},
		{"a single quote", "2.328.0'"},
		{"a newline and a second command", "2.328.0\npoweroff"},
		// THE ANCHORING IS GO'S, AND THAT IS NOT UNIVERSAL. In Perl and PCRE `$`
		// matches BEFORE a final newline, so this exact pattern would admit a
		// second command in those languages. Go's `$` is end-of-text without
		// `(?m)`. Measured, and pinned here so a future rewrite of the pattern in
		// another dialect -- or the addition of `(?m)` -- is caught.
		{"a trailing newline", "2.328.0\n"},
		{"a newline hiding a command before the anchor", "2.328.0\npoweroff\n"},
		{"a leading newline", "\n2.328.0"},
		{"a trailing carriage return", "2.328.0\r"},
		// `\d` IS ASCII IN GO, not Unicode. Also measured: these would otherwise
		// reach a URL path as bytes no release ever had.
		{"Arabic-Indic digits", "٢.٣٢٨.٠"},
		{"fullwidth digits", "２.３２８.０"},
		{"a NUL byte in the suffix", "2.328.0-\x00"},
		{"whitespace splitting the curl arguments", "2.328.0 /etc/shadow"},
		{"a path traversal into another release", "../../../../etc/passwd"},
		{"a semicolon", "2.328.0; poweroff"},
		{"an ampersand", "2.328.0 && poweroff"},
		{"a shell variable", "$RUNNER"},
		{"a glob", "2.328.*"},
		{"not a version at all", "latest"},
		{"two components", "2.328"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

			f := newFakeEC2(t)
			f.respond = b.reply

			p := newTestProvider(t, f, nil)

			_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
				BaseImage: "ami-base", InstanceType: "c7i.xlarge",
				Arch: "x64", RunnerVersion: tc.version, Name: "test-image",
			})
			if err == nil {
				t.Fatalf("--runner-version %q built an image; it is interpolated into a URL "+
					"in a script that runs as root", tc.version)
			}

			// NOTHING WAS LAUNCHED, which the error alone does not say. A refusal
			// raised after RunInstances would already have run the payload.
			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("%d builders were launched for --runner-version %q", n, tc.version)
			}

			// AND NOTHING WAS IMAGED. The payload that matters is `poweroff`, which
			// billet reads as a finished build.
			if n := f.countOf("CreateImage"); n != 0 {
				t.Errorf("%d images were registered for --runner-version %q", n, tc.version)
			}
		})
	}
}

// THE OTHER DIRECTION, so the guard is not simply "refuse everything". A rule
// that rejected real releases would be discovered by an operator at the moment
// they needed to bump the runner, which is the worst time to find it.
func TestTheVersionsGitHubActuallyPublishesAreAccepted(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"2.328.0",
		"2.336.0",
		"2.300.2",
		"3.0.0",
		"2.328.0-rc.1",
		"2.328.0+build.5",
	} {
		script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest, RunnerVersion: version, Arch: "x64"})
		if err != nil {
			t.Fatalf("provisionScript refused the release version %q: %v", version, err)
		}

		if !strings.Contains(script, "actions-runner-linux-x64-"+version+".tar.gz") {
			t.Errorf("the script for %q does not install that release", version)
		}
	}
}
