package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE JDK DRIVER RUNS UNDER pipefail, AND ITS FIRST COMMAND IS A PIPELINE.
//
// Adoptium's signing key is fetched with `curl … | gpg --dearmor > keyring`.
// Without pipefail a pipeline's status is its LAST command's, so a curl that 404s
// or times out leaves gpg writing a ZERO-BYTE keyring and `bash -eux` carrying
// on. Measured: the line after `false | cat` runs, the driver exits 0, and the
// keyring file is there and empty. apt then fails on an unsigned repository —
// fifteen minutes and one paid builder into an EC2 build, reading as an apt
// problem rather than a fetch one.
//
// THIS MATTERED LESS BEFORE THIS BRANCH. In the guest build the whole thing runs
// in a chroot on a machine somebody is watching; on the EC2 builder it is
// unattended and billable. Extracting the installer put the same lines on both
// paths, which is the point — and is why the weaker of the two options is not
// good enough for either.
func TestTheJDKDriverRunsUnderPipefail(t *testing.T) {
	t.Parallel()

	asset := readScriptFile(t, toolcacheAssetPath)

	var invocation string

	for _, line := range strings.Split(asset, "\n") {
		if strings.Contains(line, "/bin/bash") && strings.Contains(line, "<<'JAVA'") {
			invocation = strings.TrimSpace(line)

			break
		}
	}

	if invocation == "" {
		t.Fatal("the JDK installer no longer runs a bash driver with a JAVA heredoc; this " +
			"test is about that driver's options and cannot find it")
	}

	if !strings.Contains(invocation, "pipefail") {
		t.Errorf("the JDK driver runs as %q, with no pipefail — and its first command is "+
			"`curl | gpg`, so a failed fetch writes an empty keyring and the build carries "+
			"on to fail on an unsigned repository", invocation)
	}

	// AND THE OPTIONS ACTUALLY DO WHAT THE NAME SUGGESTS, on this machine's bash,
	// rather than being a flag nobody checked. The difference is the whole reason
	// the line was changed.
	opts := ""

	for _, f := range strings.Fields(invocation) {
		if strings.HasPrefix(f, "-") && strings.Contains(f, "e") && f != "-s" {
			opts = f

			break
		}
	}

	if opts == "" {
		t.Fatalf("could not find the option word in %q", invocation)
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "reached")

	// A pipeline whose FIRST stage fails and whose last succeeds — the shape of
	// `curl | gpg` when the fetch is what went wrong.
	probe := "false | cat >/dev/null\ntouch " + marker + "\n"

	cmd := exec.CommandContext(t.Context(), "bash", opts, "-c", probe)
	if err := cmd.Run(); err == nil {
		t.Errorf("bash %s did not abort on a pipeline whose first stage failed", opts)
	}

	if _, err := os.Stat(marker); err == nil {
		t.Errorf("bash %s ran the line after a failed fetch; the keyring would be empty "+
			"and the build would continue", opts)
	}
}

// AND EVERY apt TRANSACTION WAITS FOR THE dpkg LOCK.
//
// On the guest build these run inside a chroot, where no apt-daily.timer,
// apt-daily-upgrade.timer or unattended-upgrades.service exists. On the EC2
// builder they run on the LIVE system, as root, in a window from about minute
// three to about minute twenty after boot — squarely inside the period when
// Ubuntu's `Persistent=true` apt timers fire.
//
// build.go learned this for its own transaction and says so; extracting the
// installers put four more onto the same live builder without it. The failure is
// `Could not get lock /var/lib/dpkg/lock-frontend`, arriving fifteen minutes and
// one paid c7i.xlarge into a build, reading as an apt problem.
func TestEveryToolcacheAptTransactionWaitsForTheLock(t *testing.T) {
	t.Parallel()

	asset := readScriptFile(t, toolcacheAssetPath)

	var bare []string

	for i, line := range strings.Split(asset, "\n") {
		trimmed := strings.TrimSpace(line)

		if !strings.HasPrefix(trimmed, "apt-get ") {
			continue
		}

		// `apt-get clean` and `apt-get autoremove` take no lock worth waiting on
		// and are not what a timer collides with.
		if strings.Contains(trimmed, "apt-get clean") {
			continue
		}

		if !strings.Contains(trimmed, "DPkg::Lock::Timeout") {
			bare = append(bare, strings.TrimSpace(line)+"  (line "+strconv.Itoa(i+1)+")")
		}
	}

	if len(bare) > 0 {
		t.Errorf("these apt transactions do not wait for the dpkg lock, and on the EC2 "+
			"builder they run on a live system while Ubuntu's apt timers are firing:\n  %s",
			strings.Join(bare, "\n  "))
	}
}
