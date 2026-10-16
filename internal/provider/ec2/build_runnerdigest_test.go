package ec2

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/runnerrelease"
)

// THE RUNNER IS CHECKED BEFORE IT IS UNPACKED, and the check is run, not read.
//
// The fragment is lifted out of the rendered script and executed against a real
// tarball and a tampered one, under every shell that might run it, WITHOUT
// `set -e` as well as with it: the script runs under `set -eux`, and a check
// that only holds because of it is a check one edit away from a `;` chain that
// prints FAILED and extracts anyway. The line after the fragment stands in for
// the rest of the script, poweroff included, so "the build stopped" is observed
// rather than inferred from an exit status.
func TestTheRunnerIsCheckedBeforeItIsUnpacked(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum is not on PATH; the builder is Ubuntu, where it always is")
	}

	genuine := runnerTarball(t, "#!/bin/sh\necho the runner\n")
	tampered := runnerTarball(t, "#!/bin/sh\necho somebody else's runner\n")

	sum := sha256.Sum256(genuine)
	digest := hex.EncodeToString(sum[:])

	script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL, payloadSHA256: testPayloadDigest,
		RunnerSHA256: digest, RunnerVersion: "2.328.0", Arch: "x64"})
	if err != nil {
		t.Fatalf("provisionScript: %v", err)
	}

	fragment := runnerFetchBlock(t, script)

	const rest = "billet-reached-the-rest-of-the-script"

	shells := []string{"sh"}
	for _, extra := range []string{"dash", "bash"} {
		if _, err := exec.LookPath(extra); err == nil {
			shells = append(shells, extra)
		}
	}

	for _, tc := range []struct {
		name    string
		tarball []byte
		// download false makes the fake curl fail as a refused download does.
		download bool
		wantOK   bool
	}{
		{name: "the release billet pins", tarball: genuine, download: true, wantOK: true},
		{name: "a tarball that is not that release", tarball: tampered, download: true},
		{name: "a download that failed", download: false},
	} {
		for _, shell := range shells {
			for _, errexit := range []bool{false, true} {
				name := tc.name + "/" + shell
				if errexit {
					name += "/set -e"
				}

				t.Run(name, func(t *testing.T) {
					t.Parallel()

					src := t.TempDir()
					work := t.TempDir()

					fixture := ""
					if tc.download {
						fixture = filepath.Join(src, "served.tar.gz")
						if err := os.WriteFile(fixture, tc.tarball, 0o600); err != nil {
							t.Fatalf("write the served tarball: %v", err)
						}
					}

					urlFile := filepath.Join(src, "url")

					// A curl FUNCTION, not an executable on PATH: a parallel test's
					// fork can hold the descriptor that wrote one, and exec then fails
					// with ETXTBSY (see TestTheFreeSpaceGuardRefusesWhatItCannotRead).
					fake := "curl() {\n" +
						"  out=''\n" +
						"  while [ \"$#\" -gt 0 ]; do\n" +
						"    case \"$1\" in\n" +
						"      -o) out=$2; shift ;;\n" +
						"      https://*) printf '%s\\n' \"$1\" > \"$BILLET_TEST_URL\" ;;\n" +
						"    esac\n" +
						"    shift\n" +
						"  done\n" +
						"  [ -n \"$BILLET_TEST_FIXTURE\" ] || return 22\n" +
						"  cp \"$BILLET_TEST_FIXTURE\" \"$out\"\n" +
						"}\n"

					prelude := ""
					if errexit {
						prelude = "set -e\n"
					}

					cmd := exec.CommandContext(t.Context(), shell, "-c",
						prelude+fake+fragment+"echo "+rest+"\n")
					cmd.Dir = work
					cmd.Env = append(os.Environ(),
						"BILLET_TEST_FIXTURE="+fixture, "BILLET_TEST_URL="+urlFile)

					out, err := cmd.CombinedOutput()

					_, statErr := os.Stat(filepath.Join(work, "run.sh"))
					extracted := statErr == nil

					if tc.wantOK {
						if err != nil {
							t.Fatalf("the check refused the release it pins: %v\n%s", err, out)
						}

						if !extracted {
							t.Fatalf("the check passed and nothing was unpacked\n%s", out)
						}

						if _, err := os.Stat(filepath.Join(work, "runner.tar.gz")); !os.IsNotExist(err) {
							t.Errorf("the tarball was left behind in the image (stat: %v)", err)
						}

						if !strings.Contains(string(out), rest) {
							t.Errorf("the script stopped after a runner that verified\n%s", out)
						}

						url, err := os.ReadFile(urlFile)
						if err != nil {
							t.Fatalf("the fake curl recorded no URL: %v", err)
						}

						want := "https://github.com/actions/runner/releases/download/v2.328.0/" +
							"actions-runner-linux-x64-2.328.0.tar.gz\n"
						if string(url) != want {
							t.Errorf("the runner was fetched from %q, want %q", url, want)
						}

						return
					}

					if err == nil {
						t.Fatalf("the fragment exited 0 for %s; the build would go on to "+
							"poweroff and billet would image it\n%s", tc.name, out)
					}

					if extracted {
						t.Errorf("a runner was unpacked from %s\n%s", tc.name, out)
					}

					if strings.Contains(string(out), rest) {
						t.Errorf("the script carried on past %s, so the rest of the "+
							"build, poweroff included, would run\n%s", tc.name, out)
					}

					// THE DIAGNOSTIC, where the check is what refused: a fixture this
					// test broke would also exit non-zero, for some other reason.
					if tc.download && !strings.Contains(string(out), "does not hash to "+digest) {
						t.Errorf("the refusal does not name the digest the tarball "+
							"failed\n%s", out)
					}
				})
			}
		}
	}
}

// AND THE DIGEST IS THE ONE THE SPEC CARRIES, for the platform the spec names.
// An arm64 image checked against x64's digest fails every build; one whose script
// fetched x64's tarball would install a runner that cannot execute.
func TestTheScriptChecksTheSpecsDigestForItsPlatform(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ arch, platform string }{
		{"x64", "linux-x64"},
		{"arm64", "linux-arm64"},
	} {
		digest := strings.Repeat("ab", 32)

		script, err := provisionScript(BuildSpec{payloadURL: testPayloadURL,
			payloadSHA256: testPayloadDigest, RunnerSHA256: digest,
			RunnerVersion: "2.328.0", Arch: tc.arch})
		if err != nil {
			t.Fatalf("provisionScript(%s): %v", tc.arch, err)
		}

		block := runnerFetchBlock(t, script)

		for _, want := range []string{
			"actions-runner-" + tc.platform + "-2.328.0.tar.gz",
			"printf '%s  %s\\n' '" + digest + "' runner.tar.gz | sha256sum -c -",
		} {
			if !strings.Contains(block, want) {
				t.Errorf("the %s fetch is missing %q:\n%s", tc.arch, want, block)
			}
		}
	}
}

// A BUILD THAT CANNOT CHECK ITS RUNNER IS REFUSED BEFORE A BUILDER EXISTS.
//
// The digest is written into a script that runs as root, so it is held to the
// shape sha256sum prints rather than quoted; and a missing one is refused here
// rather than rendered as a check sha256sum rejects forty paid minutes later.
func TestARunnerDigestThatIsNotOneIsRefusedBeforeAnyLaunch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, digest string }{
		{"absent", ""},
		{"command substitution", "$(poweroff)"},
		{"one digit short", strings.Repeat("a", 63)},
		{"one digit long", strings.Repeat("a", 65)},
		{"uppercase", strings.Repeat("A", 64)},
		{"not hex", strings.Repeat("g", 64)},
		{"a quote and a second command", strings.Repeat("a", 60) + "'; poweroff; '"},
		{"a trailing newline", strings.Repeat("a", 64) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			b := &buildFake{stopReason: "Client.InstanceInitiatedShutdown", imageState: "available"}

			f := newFakeEC2(t)
			f.respond = b.reply

			p := newTestProvider(t, f, nil)

			_, err := p.BuildImage(t.Context(), BuildSpec{payloadURL: testPayloadURL,
				payloadSHA256: testPayloadDigest, BaseImage: "ami-base", InstanceType: "c7i.xlarge",
				Arch: "x64", RunnerVersion: "2.328.0", RunnerSHA256: tc.digest, Name: "test-image",
			})
			if err == nil {
				t.Fatalf("a build with runner digest %q went ahead", tc.digest)
			}

			if !strings.Contains(err.Error(), "the runner's sha256") {
				t.Errorf("the refusal does not say it is about the runner's digest: %v", err)
			}

			if n := f.countOf("RunInstances"); n != 0 {
				t.Errorf("%d builders were launched for runner digest %q", n, tc.digest)
			}
		})
	}
}

// PinnedRunnerSHA256 IS runnerrelease's ANSWER, read through the platform map,
// so the AMI and the guest image check one digest.
func TestThePinnedRunnerDigestIsTheGuestBuildsPin(t *testing.T) {
	t.Parallel()

	for arch, platform := range map[string]string{"x64": "linux-x64", "arm64": "linux-arm64"} {
		want, ok := runnerrelease.PinnedSHA256For(platform)
		if !ok {
			t.Fatalf("runnerrelease pins nothing for %s", platform)
		}

		got, ok := PinnedRunnerSHA256(arch)
		if !ok || got != want {
			t.Errorf("PinnedRunnerSHA256(%s) = %q, %v; runnerrelease pins %q for %s",
				arch, got, ok, want, platform)
		}

		if !IsRunnerSHA256(got) {
			t.Errorf("the pinned %s digest %q is not one a build would accept", arch, got)
		}
	}

	if got, ok := PinnedRunnerSHA256("amd64"); ok || got != "" {
		t.Errorf("PinnedRunnerSHA256(amd64) = %q, %v; an architecture billet does not map has "+
			"no digest", got, ok)
	}
}

// runnerFetchBlock lifts the runner's download, check and unpack out of the
// generated script: from the curl that fetches it to the rm that removes it.
//
// It refuses a block with no check inside it, so a script that lost the check
// fails here by name rather than in whichever assertion happens to notice.
func runnerFetchBlock(t *testing.T, script string) string {
	t.Helper()

	lines := strings.Split(script, "\n")
	start := firstLineOf(t, lines, "curl -fsSL -o runner.tar.gz ")

	for i := start; i < len(lines); i++ {
		if lines[i] != "rm runner.tar.gz" {
			continue
		}

		block := strings.Join(lines[start:i+1], "\n") + "\n"
		if !strings.Contains(block, "sha256sum -c -") {
			t.Fatalf("the runner is fetched and unpacked with no check between:\n%s", block)
		}

		return block
	}

	t.Fatal("the runner's fetch has no `rm runner.tar.gz` after it")

	return ""
}

// runnerTarball is a gzipped tar shaped like an actions/runner release: run.sh at
// its root.
func runnerTarball(t *testing.T, runSH string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	if err := tw.WriteHeader(&tar.Header{Name: "run.sh", Mode: 0o755, Size: int64(len(runSH)),
		Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("tar header: %v", err)
	}

	if _, err := tw.Write([]byte(runSH)); err != nil {
		t.Fatalf("tar body: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}

	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return buf.Bytes()
}
