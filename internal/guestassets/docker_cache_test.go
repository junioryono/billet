package guestassets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerCachePublishesOnlyAnAuthoritativeCleanSuccess(t *testing.T) {
	t.Parallel()
	if !strings.Contains(DockerCacheScript, "{{.Repository}} {{.Tag}} {{.Digest}} {{.ID}}") {
		t.Fatal("the Docker inventory cannot detect a tag-only change")
	}

	for _, tc := range []struct {
		name      string
		status    string
		before    string
		after     string
		afterExit string
		sortExit  string
		cmpExit   string
		endpoint  string
	}{
		{name: "changed success", status: "100", before: "old", after: "new", afterExit: "0", endpoint: "/v1/docker-store/ready"},
		{name: "unchanged success", status: "100", before: "same", after: "same", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
		{name: "inventory failure", status: "100", before: "old", after: "new", afterExit: "1", endpoint: "/v1/volumes/0/discard"},
		{name: "sort failure", status: "100", before: "old", after: "new", afterExit: "0", sortExit: "2", endpoint: "/v1/volumes/0/discard"},
		{name: "comparison failure", status: "100", before: "old", after: "new", afterExit: "0", cmpExit: "2", endpoint: "/v1/volumes/0/discard"},
		{name: "success with issues", status: "101", before: "old", after: "new", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
		{name: "failed", status: "102", before: "old", after: "new", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
		{name: "cancelled", status: "103", before: "old", after: "new", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
		{name: "unknown runner exit", status: "7", before: "old", after: "new", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := t.TempDir()
			if err := os.WriteFile(filepath.Join(state, "slot"), []byte("0\n"), 0o600); err != nil {
				t.Fatalf("write slot: %v", err)
			}
			if err := os.WriteFile(filepath.Join(state, "device"), []byte("/dev/test-cache\n"), 0o600); err != nil {
				t.Fatalf("write device: %v", err)
			}
			if err := os.WriteFile(filepath.Join(state, "images-before"), []byte(tc.before+"\n"), 0o600); err != nil {
				t.Fatalf("write image inventory: %v", err)
			}

			shadow := t.TempDir()
			logPath := filepath.Join(shadow, "curl.log")
			writeStub(t, shadow, "systemctl", "exit 0\n")
			writeStub(t, shadow, "sync", "exit 0\n")
			writeStub(t, shadow, "umount", "exit 0\n")
			writeStub(t, shadow, "e2fsck", "exit 0\n")
			writeStub(t, shadow, "docker", "printf '%s\\n' \"$BILLET_TEST_IMAGES_AFTER\"\nexit \"$BILLET_TEST_DOCKER_EXIT\"\n")
			writeStub(t, shadow, "sort", "test -z \"$BILLET_TEST_SORT_EXIT\" || exit \"$BILLET_TEST_SORT_EXIT\"\nexec /usr/bin/sort \"$@\"\n")
			writeStub(t, shadow, "cmp", "test -z \"$BILLET_TEST_CMP_EXIT\" || exit \"$BILLET_TEST_CMP_EXIT\"\nexec /usr/bin/cmp \"$@\"\n")
			writeStub(t, shadow, "blkid", `
case "$*" in
  *TYPE*) printf 'ext4\n' ;;
  *UUID*) printf 'test-uuid\n' ;;
  *) exit 1 ;;
esac
`)
			writeStub(t, shadow, "curl", `
printf '%s\n' "$*" >> "$BILLET_TEST_CURL_LOG"
printf '{"ready":true}\n'
`)

			script := filepath.Join(shadow, "docker-cache.sh")
			if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
				t.Fatalf("write Docker cache script: %v", err)
			}
			run := exec.CommandContext(t.Context(), script, "complete", tc.status)
			run.Env = append(os.Environ(),
				"PATH="+shadow+":"+os.Getenv("PATH"),
				"BILLET_CACHE_ENDPOINT=http://cache.test",
				"BILLET_CACHE_TOKEN=test-token",
				"BILLET_DOCKER_CACHE_STATE_DIR="+state,
				"BILLET_TEST_IMAGES_AFTER="+tc.after,
				"BILLET_TEST_DOCKER_EXIT="+tc.afterExit,
				"BILLET_TEST_SORT_EXIT="+tc.sortExit,
				"BILLET_TEST_CMP_EXIT="+tc.cmpExit,
				"BILLET_TEST_CURL_LOG="+logPath,
			)
			if output, err := combinedOutputRetry(t, run); err != nil {
				t.Fatalf("complete returned %v; cache cleanup must not change the job result:\n%s", err, output)
			}

			called, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read cache request: %v", err)
			}
			if !strings.Contains(string(called), tc.endpoint) {
				t.Errorf("status %s with inventories %q/%q called:\n%s\nwant %s",
					tc.status, tc.before, tc.after, called, tc.endpoint)
			}
			if tc.endpoint == "/v1/docker-store/ready" &&
				!strings.Contains(string(called), "Authorization: Bearer test-token") {
				t.Errorf("readiness call did not use the guest session bearer:\n%s", called)
			}
		})
	}
}

// THE GUEST WAITS FOR THE NODE TO OPEN SETTLEMENT, HOWEVER LATE, UP TO ITS BOUND: a
// node runs its destroys one at a time, so a job's settlement can open minutes after the
// job ended, behind another job's publication, and a guest that stops asking discards
// the store the node is about to publish. Past the bound it gives the store back.
func TestDockerCacheWaitsForALateSettlement(t *testing.T) {
	t.Parallel()
	if !strings.Contains(DockerCacheScript, "asks=${BILLET_DOCKER_SETTLE_ASKS:-3600}") {
		t.Fatal("the guest's default bound is no longer 3600 asks, an hour at a second apart")
	}
	for _, tc := range []struct {
		name string
		// refusals is how many readiness asks the node answers 409 before it opens.
		refusals string
		// bound is BILLET_DOCKER_SETTLE_ASKS, empty for the script's own.
		bound     string
		wantAsks  int
		discarded bool
	}{
		// Four times the 100 asks that lost arm64's store, under the default bound.
		{name: "opened after 400 refusals", refusals: "400", wantAsks: 401},
		{name: "never opened within the bound", refusals: "1000000", bound: "5", wantAsks: 5, discarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			state := t.TempDir()
			for name, body := range map[string]string{
				"slot": "0\n", "device": "/dev/test-cache\n", "images-before": "old\n",
			} {
				if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o600); err != nil {
					t.Fatalf("write %s: %v", name, err)
				}
			}

			shadow := t.TempDir()
			logPath := filepath.Join(shadow, "curl.log")
			asks := filepath.Join(shadow, "asks")
			for _, name := range []string{"systemctl", "sync", "umount", "e2fsck"} {
				writeStub(t, shadow, name, "exit 0\n")
			}
			// THE SECOND BETWEEN ASKS IS WHAT MAKES THE COUNT AN HOUR, so every sleep is
			// recorded with what it was asked.
			sleeps := filepath.Join(shadow, "sleeps")
			writeStub(t, shadow, "sleep", "printf '%s\\n' \"$*\" >> \"$BILLET_TEST_SLEEPS\"\n")
			writeStub(t, shadow, "docker", "printf 'new\\n'\n")
			writeStub(t, shadow, "blkid", "printf 'x\\n'\n")
			writeStub(t, shadow, "curl", `
printf '%s\n' "$*" >> "$BILLET_TEST_CURL_LOG"
case "$*" in
*/v1/docker-store/ready*)
	printf x >> "$BILLET_TEST_ASKS"
	if [ "$(wc -c < "$BILLET_TEST_ASKS")" -le "$BILLET_TEST_REFUSALS" ]; then
		exit 22
	fi
	printf '{"ready":true}\n' ;;
esac
`)

			script := filepath.Join(shadow, "docker-cache.sh")
			if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
				t.Fatalf("write Docker cache script: %v", err)
			}
			run := exec.CommandContext(t.Context(), script, "complete", "100")
			run.Env = append(os.Environ(),
				"PATH="+shadow+":"+os.Getenv("PATH"),
				"BILLET_CACHE_ENDPOINT=http://cache.test",
				"BILLET_CACHE_TOKEN=test-token",
				"BILLET_DOCKER_CACHE_STATE_DIR="+state,
				"BILLET_TEST_CURL_LOG="+logPath,
				"BILLET_TEST_ASKS="+asks,
				"BILLET_TEST_REFUSALS="+tc.refusals,
				"BILLET_TEST_SLEEPS="+sleeps,
				// ALWAYS SET, empty for the script's own bound, so an inherited value
				// never stands in for the default.
				"BILLET_DOCKER_SETTLE_ASKS="+tc.bound,
			)
			output, err := combinedOutputRetry(t, run)
			if err != nil {
				t.Fatalf("complete returned %v:\n%s", err, output)
			}
			called, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read cache requests: %v", err)
			}
			asked, err := os.ReadFile(asks)
			if err != nil {
				t.Fatalf("read the readiness asks: %v", err)
			}
			if len(asked) != tc.wantAsks {
				t.Errorf("the guest asked for readiness %d times, want %d:\n%s", len(asked), tc.wantAsks, output)
			}
			if got := strings.Contains(string(called), "/v1/volumes/0/discard"); got != tc.discarded {
				t.Errorf("the guest gave the store back: %t, want %t:\n%s", got, tc.discarded, output)
			}
			// ONE SECOND AFTER EVERY REFUSED ASK, and none after the one that was answered.
			slept, err := os.ReadFile(sleeps)
			if err != nil {
				t.Fatalf("read the sleeps: %v", err)
			}
			refused := tc.wantAsks
			if !tc.discarded {
				refused--
			}
			if want := strings.Repeat("1\n", refused); string(slept) != want {
				t.Errorf("the guest slept %d times (%q...), want %d sleeps of a second",
					strings.Count(string(slept), "\n"), firstLine(string(slept)), refused)
			}
		})
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")

	return line
}

func TestDockerCachePreservesTheRunnerServiceExitContract(t *testing.T) {
	t.Parallel()

	shadow := t.TempDir()
	script := filepath.Join(shadow, "docker-cache.sh")
	if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
		t.Fatalf("write Docker cache script: %v", err)
	}
	for status, want := range map[string]string{
		"100": "0", "101": "0", "102": "0", "103": "0", "104": "0", "105": "0",
		"1": "1", "7": "7", "": "1", "not-a-status": "1",
	} {
		run := exec.CommandContext(t.Context(), script, "service-status", status)
		output, err := outputRetry(t, run)
		if err != nil {
			t.Fatalf("service-status %q: %v", status, err)
		}
		if got := strings.TrimSpace(string(output)); got != want {
			t.Errorf("service-status %q = %q, want %q", status, got, want)
		}
	}
}

func TestDockerCacheDoesNotFormatAfterABlkidError(t *testing.T) {
	t.Parallel()

	shadow := t.TempDir()
	mkfsLog := filepath.Join(shadow, "mkfs.log")
	writeStub(t, shadow, "blkid", "exit 1\n")
	writeStub(t, shadow, "mkfs.ext4", "printf called > \"$BILLET_TEST_MKFS_LOG\"\n")
	script := filepath.Join(shadow, "docker-cache.sh")
	if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
		t.Fatalf("write Docker cache script: %v", err)
	}
	run := exec.CommandContext(t.Context(), script, "prepare-filesystem", "/dev/test-cache")

	// THE SHADOW IS THE WHOLE WORLD HERE, AND THE HOST'S PATH IS DELIBERATELY NOT
	// APPENDED. `prepare-filesystem` runs exactly two external commands and both are
	// stubbed above, so nothing is missing -- and inheriting the host's PATH leaves a
	// REAL blkid reachable behind the stub. That is not hypothetical tidiness: this
	// test asks what happens when blkid cannot read the device, and MEASURED on
	// ubuntu-24.04, the real `blkid -o value -s TYPE /dev/test-cache` exits 2 for a
	// device that does not exist. Two is blkid's "no signature here", which this
	// script reads as a blank volume and formats -- so the moment the stub is not the
	// one that runs, the host answers with the single status that turns this test's
	// subject into its opposite, the script exits 0, and the failure reads as billet
	// having accepted an I/O error.
	//
	// Whatever keeps a stub from running -- ETXTBSY from a parallel fork, which is
	// what retryETXTBSY exists for one file over, or anything else -- must surface as
	// "not found" rather than as a real tool's opinion. A closed PATH is what makes
	// that true by construction.
	//
	// The other tests in this file cannot do the same: `complete` shells out to jq,
	// which is not stubbed, so they need the host's PATH and are not exposed this way.
	run.Env = append(os.Environ(),
		"PATH="+shadow,
		"BILLET_TEST_MKFS_LOG="+mkfsLog,
	)

	output, err := combinedOutputRetry(t, run)

	// FORMATTED ONCE, because the first assertion below is the one that fired in CI
	// and it printed NOTHING -- not the output, not whether mkfs ran -- which is why
	// the run said only that the script exited 0 and left nothing to reason from. An
	// assertion about a subprocess has to carry what the subprocess did.
	_, mkfsErr := os.Stat(mkfsLog)
	evidence := fmt.Sprintf("script exit: %v\nmkfs called: %t\noutput:\n%s",
		err, mkfsErr == nil, output)

	if err == nil {
		t.Fatalf("a blkid I/O error was accepted as a blank cache volume\n%s", evidence)
	}
	if !strings.Contains(string(output), "signature could not be read") {
		t.Fatalf("blkid failure returned the wrong reason\n%s", evidence)
	}
	if !os.IsNotExist(mkfsErr) {
		t.Fatalf("mkfs was called after blkid failed\n%s", evidence)
	}
}

func TestDockerCacheRetainsCustodyWhenAStartedStoreCannotUnmount(t *testing.T) {
	t.Parallel()

	shadow := t.TempDir()
	state := filepath.Join(shadow, "state")
	calls := filepath.Join(shadow, "systemctl.log")
	writeStub(t, shadow, "systemctl", `
printf '%s\n' "$*" >> "$BILLET_TEST_SYSTEMCTL_LOG"
test "$(wc -l < "$BILLET_TEST_SYSTEMCTL_LOG")" -ge 2
`)
	writeStub(t, shadow, "umount", "exit 1\n")
	writeStub(t, shadow, "curl", "printf called > \"$BILLET_TEST_CURL_LOG\"\n")
	script := filepath.Join(shadow, "docker-cache.sh")
	if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
		t.Fatalf("write Docker cache script: %v", err)
	}
	curlLog := filepath.Join(shadow, "curl.log")
	run := exec.CommandContext(t.Context(), script, "activate-store", "0", "/dev/test-cache")
	run.Env = append(os.Environ(),
		"PATH="+shadow+":"+os.Getenv("PATH"),
		"BILLET_CACHE_ENDPOINT=http://cache.test",
		"BILLET_CACHE_TOKEN=test-token",
		"BILLET_DOCKER_CACHE_STATE_DIR="+state,
		"BILLET_TEST_CURL_LOG="+curlLog,
		"BILLET_TEST_SYSTEMCTL_LOG="+calls,
	)
	output, err := combinedOutputRetry(t, run)
	if err != nil {
		t.Fatalf("a retryable Docker start failure prevented the job from continuing: %v\n%s", err, output)
	}
	if _, err := os.Stat(curlLog); !os.IsNotExist(err) {
		t.Fatalf("the still-mounted store was detached through the cache API: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "slot")); err != nil {
		t.Fatalf("durable cache custody was not retained for completion: %v", err)
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := writeExecutable(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write %s stub: %v", name, err)
	}
}

// AN IMAGE THAT ENABLES DOCKER AT BOOT HAS A DAEMON THAT READ ITS CONFIGURATION BEFORE THE
// BOOT CHANGED IT, and `start` on a running service does nothing, so a cold path must
// restart it when told. A container would otherwise be answered that the npm registry
// is its own loopback (measured in a guest, 2026-10-23). Told nothing, the helper starts
// it, which is all a Firecracker guest's has ever done.
func TestDockerCacheRestartsADaemonThatMissedTheBootsConfiguration(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, changed, want string
	}{
		{"told the configuration changed", "1", "restart docker.service\n"},
		{"told nothing", "", "start docker.service\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shadow := t.TempDir()
			calls := filepath.Join(shadow, "systemctl.log")
			writeStub(t, shadow, "systemctl", "printf '%s\\n' \"$*\" >> \"$BILLET_TEST_SYSTEMCTL_LOG\"\n")
			script := filepath.Join(shadow, "docker-cache.sh")
			if err := writeExecutable(script, []byte(DockerCacheScript), 0o755); err != nil {
				t.Fatalf("write Docker cache script: %v", err)
			}
			run := exec.CommandContext(t.Context(), script, "prepare")
			run.Env = append(os.Environ(),
				"PATH="+shadow+":"+os.Getenv("PATH"),
				"BILLET_CACHE_ENDPOINT=", "BILLET_CACHE_TOKEN=",
				"BILLET_DOCKER_CONFIG_CHANGED="+tc.changed,
				"BILLET_TEST_SYSTEMCTL_LOG="+calls,
			)
			if output, err := combinedOutputRetry(t, run); err != nil {
				t.Fatalf("prepare: %v\n%s", err, output)
			}
			got, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("systemctl was asked %q, want %q", got, tc.want)
			}
		})
	}
}
