package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THE IMAGE'S SUDOERS DROP-IN IS INSTALLED, AND THE BUILD GOES ON ONLY IF SUDO
// READS IT. The build's own function runs here against a fixture image, through
// the real visudo, with chroot standing in by checking the fixture's whole
// policy: the drop-in lands mode 0440 with exactly the cache's two variables and
// the four that name the trust bundle, kept only at billet's own two paths of it,
// one sudo cannot parse ends the build, and
// so does a policy that never includes it, which would leave root's git without
// a session and root's npm without the node's authority again.
func TestTheImagesSudoersKeepsTheCacheVariablesAcrossSudo(t *testing.T) {
	t.Parallel()

	// THE LINUX BUILDER'S: the build installs from a here-document on stdin,
	// which GNU install reads and BSD install refuses.
	if runtime.GOOS != "linux" {
		t.Skip("the guest image is built on Linux")
	}
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo is not installed here; the build checks the drop-in in the image")
	}
	build := readScriptFile(t, "build-guest-image.sh")
	_, rest, ok := strings.Cut(build, "\ninstall_cache_sudoers() {\n")
	if !ok {
		t.Fatal("the build has no install_cache_sudoers")
	}
	body, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatal("install_cache_sudoers has no end")
	}
	function := "install_cache_sudoers() {\n" + body + "\n}\n"

	run := func(t *testing.T, function, policy string) (string, error) {
		t.Helper()

		rootfs := t.TempDir()
		if err := os.MkdirAll(filepath.Join(rootfs, "etc", "sudoers.d"), 0o750); err != nil {
			t.Fatal(err)
		}
		policy = strings.ReplaceAll(policy, "ROOT", rootfs)
		if err := os.WriteFile(filepath.Join(rootfs, "etc", "sudoers"), []byte(policy), 0o440); err != nil {
			t.Fatal(err)
		}
		script := strings.Join([]string{
			"set -euo pipefail",
			// chroot as the build calls it, `chroot ROOT visudo -c`: the
			// fixture's whole policy, read from its own path.
			`chroot() { local root=$1; shift; [ "$*" = "visudo -c" ] || return 99; ` +
				shellQuote(visudo) + ` -c -f "$root/etc/sudoers"; }`,
			function,
			"install_cache_sudoers " + shellQuote(rootfs),
			"echo completed",
		}, "\n")
		output, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
		if err == nil {
			path := filepath.Join(rootfs, "etc", "sudoers.d", "billet-cache")
			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o440 {
				t.Errorf("the drop-in is mode %o, want 0440", info.Mode().Perm())
			}
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if want := wantSudoersDropIn(); string(raw) != want {
				t.Errorf("the drop-in is %q, want %q", raw, want)
			}
		}

		return string(output), err
	}

	const including = "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n@includedir ROOT/etc/sudoers.d\n"
	if output, err := run(t, function, including); err != nil || !strings.Contains(output, "completed") {
		t.Fatalf("installing the drop-in failed: %v\n%s", err, output)
	}
	broken := strings.Replace(function, `"%s"\nDefaults`, `"%s\nDefaults`, 1)
	if broken == function {
		t.Fatal("the drop-in could not be broken for the test, so it proves nothing")
	}
	if output, err := run(t, broken, including); err == nil || strings.Contains(output, "completed") {
		t.Errorf("a drop-in sudo cannot parse did not end the build: %v\n%s", err, output)
	}
	const excluding = "Defaults env_reset\nroot ALL=(ALL:ALL) ALL\n"
	if output, err := run(t, function, excluding); err == nil || strings.Contains(output, "completed") {
		t.Errorf("a policy that never reads the drop-in did not end the build: %v\n%s", err, output)
	}
}

// wantSudoersDropIn is the drop-in the build must write: the cache's two
// variables by name, and the four that name the trust bundle only at the two
// paths billet puts it.
func wantSudoersDropIn() string {
	var keep []string
	for _, name := range []string{"NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		keep = append(keep, name+"="+agentTrustBundle, name+"="+hookTrustBundle)
	}

	return "Defaults env_keep += \"BILLET_CACHE_ENDPOINT BILLET_CACHE_TOKEN\"\n" +
		"Defaults env_keep += \"" + strings.Join(keep, " ") + "\"\n"
}

// The two paths of the guest's trust bundle: the agent's, and the copy the
// job-started hook makes in RUNNER_TEMP, which is the runner's _work/_temp
// (read from a real job's environment, 2026-10-23).
const (
	agentTrustBundle = "/home/runner/runner/_work/_billet/actions-cache-ca.pem"
	hookTrustBundle  = "/home/runner/runner/_work/_temp/billet-actions-cache-ca.pem"
)

// THE DROP-IN NAMES THE BUNDLE WHERE THE AGENT PUTS IT. sudo keeps a NAME=value
// entry only for that exact value, so a bundle the agent moved would cross sudo
// no more, silently; this holds the agent's two definitions to the drop-in's.
func TestTheSudoersDropInNamesTheBundleWhereTheAgentPutsIt(t *testing.T) {
	t.Parallel()

	build := readScriptFile(t, "build-guest-image.sh")
	dir, file, ok := strings.Cut(agentTrustBundle[1:], "/actions-cache-ca.pem")
	if !ok || file != "" {
		t.Fatalf("the agent's bundle path %q is not the directory and file this test reads", agentTrustBundle)
	}
	for _, definition := range []string{
		"\tactions_ca_dir=/" + dir + "\n",
		"\tactions_ca_path=\"$actions_ca_dir/actions-cache-ca.pem\"\n",
		"\ntarget=\"$RUNNER_TEMP/billet-actions-cache-ca.pem\"\n",
		"\tlocal agent_bundle=" + agentTrustBundle + "\n",
		"\tlocal hook_bundle=" + hookTrustBundle + "\n",
	} {
		if !strings.Contains(build, definition) {
			t.Errorf("the build does not define %q", strings.TrimSpace(definition))
		}
	}
}
