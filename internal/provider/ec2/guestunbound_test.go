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
// library at a path of the test's own and the system's commands stubbed, for a job with
// no cache session, the path every guest takes whatever its tier.
func TestTheGuestsBootReachesTheRunnersStart(t *testing.T) {
	t.Parallel()
	library, err := guestCachesLibrary()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "guest-caches.sh")
	writeFile(t, lib, library, 0o644)
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"docker", "systemctl", "billet-docker-cache", "sleep"} {
		writeFile(t, filepath.Join(bin, command), "#!/bin/sh\nexit 0\n", 0o755)
	}

	script := entryPointScript()
	launch := strings.Index(script, "runner_started=")
	if launch < 0 {
		t.Fatal("the entry point no longer marks the runner's start with runner_started=")
	}
	script = script[:launch] + "echo REACHED THE RUNNER\n"
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
	cmd.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.HasSuffix(string(out), "REACHED THE RUNNER\n") {
		t.Fatalf("the boot stopped before the runner's start (%v):\n%s", err, out)
	}
}
