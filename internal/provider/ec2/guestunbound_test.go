package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE GUEST'S BOOT RUNS TO THE RUNNER'S START, executed: the entry point and the guest
// caches' library run under -u, and the library is the firecracker agent's shared
// regions, so a variable the agent sets outside them is unbound here. That stopped every
// runner of the images built on 2026-11-15 (`registry_mirrors_ca_file: unbound
// variable`), and the AMI verifier, which proves the files by digest and never runs them,
// passed them. The entry point is run as written up to the runner's launch, with the
// library at a path of the test's own and the system's commands stubbed, once with no
// cache session, the path every guest takes, and once with a session serving the Go,
// Bazel and Git caches, whose reads a sessionless boot skips: what each cache configured
// is asserted too, so a boot that quietly fell back to none cannot pass.
func TestTheGuestsBootReachesTheRunnersStart(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		env  []string
		want []string
	}{
		"no cache session": {},
		"a session serving Go, Bazel and Git": {
			env: []string{"BILLET_CACHE_ENDPOINT=https://10.0.0.1:9443", "BILLET_CACHE_TOKEN=s3cret",
				"BILLET_GUEST_CACHES=go,bazel,git"},
			want: []string{
				"ENV BILLET_CACHE_ENDPOINT=https://10.0.0.1:9443", "ENV BILLET_CACHE_TOKEN=s3cret",
				"/bin/billet cache gocacheprog",
				"build --remote_cache=https://10.0.0.1:9443/v1/cas/bazel",
				`[url "https://10.0.0.1:9443/v1/git/github.com/"]`,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := runGuestBoot(t, c.env)
			for _, want := range c.want {
				if !strings.Contains(out, want) {
					t.Errorf("the boot configured no %q:\n%s", want, out)
				}
			}
		})
	}
}

// runGuestBoot runs the entry point up to the runner's launch in a directory of the test's
// own and returns what it printed, then the runner's environment and the Bazel and Git
// configuration it wrote; it fails the test if the boot stopped first.
func runGuestBoot(t *testing.T, env []string) string {
	t.Helper()
	library, err := guestCachesLibrary()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(library, guestBilletPath) {
		t.Fatalf("the library no longer names billet at %s", guestBilletPath)
	}
	lib := filepath.Join(dir, "guest-caches.sh")
	writeFile(t, lib, strings.ReplaceAll(library, guestBilletPath, filepath.Join(bin, "billet")), 0o644)
	for _, command := range []string{"billet", "billet-docker-cache", "curl", "docker", "ip", "sleep", "systemctl",
		"systemd-run"} {
		writeExecutableFile(t, filepath.Join(bin, command), "#!/bin/sh\nexit 0\n")
	}

	script := entryPointScript()
	launch := strings.Index(script, "runner_started=")
	if launch < 0 {
		t.Fatal("the entry point no longer marks the runner's start with runner_started=")
	}
	script = script[:launch] + "echo REACHED THE RUNNER\n" +
		"printf 'ENV %s\\n' \"${runner_env[@]}\"\n" +
		"cat \"$BAZELRC_FILE\" \"$GITCONFIG_FILE\" 2>/dev/null || true\n"
	for path, stand := range map[string]string{
		guestCachesPath:                      lib,
		"/usr/local/bin/billet-docker-cache": filepath.Join(bin, "billet-docker-cache"),
	} {
		if !strings.Contains(script, path) {
			t.Fatalf("the entry point no longer names %s", path)
		}
		script = strings.ReplaceAll(script, path, stand)
	}

	cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
	cmd.Env = append([]string{
		"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"BAZELRC_FILE=" + filepath.Join(dir, "bazelrc"), "GITCONFIG_FILE=" + filepath.Join(dir, "gitconfig"),
	}, env...)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "REACHED THE RUNNER\n") {
		t.Fatalf("the boot stopped before the runner's start (%v):\n%s", err, out)
	}

	return string(out)
}
