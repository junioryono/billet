package guestassets

import (
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
		endpoint  string
	}{
		{name: "changed success", status: "100", before: "old", after: "new", afterExit: "0", endpoint: "/v1/docker-store/ready"},
		{name: "unchanged success", status: "100", before: "same", after: "same", afterExit: "0", endpoint: "/v1/volumes/0/discard"},
		{name: "inventory failure", status: "100", before: "old", after: "new", afterExit: "1", endpoint: "/v1/volumes/0/discard"},
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
			if err := os.WriteFile(script, []byte(DockerCacheScript), 0o755); err != nil {
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
				"BILLET_TEST_CURL_LOG="+logPath,
			)
			if output, err := run.CombinedOutput(); err != nil {
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
		})
	}
}

func TestDockerCachePreservesTheRunnerServiceExitContract(t *testing.T) {
	t.Parallel()

	shadow := t.TempDir()
	script := filepath.Join(shadow, "docker-cache.sh")
	if err := os.WriteFile(script, []byte(DockerCacheScript), 0o755); err != nil {
		t.Fatalf("write Docker cache script: %v", err)
	}
	for status, want := range map[string]string{
		"100": "0", "101": "0", "102": "0", "103": "0", "104": "0", "105": "0",
		"1": "1", "7": "7", "": "1", "not-a-status": "1",
	} {
		run := exec.CommandContext(t.Context(), script, "service-status", status)
		output, err := run.Output()
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
	if err := os.WriteFile(script, []byte(DockerCacheScript), 0o755); err != nil {
		t.Fatalf("write Docker cache script: %v", err)
	}
	run := exec.CommandContext(t.Context(), script, "prepare-filesystem", "/dev/test-cache")
	run.Env = append(os.Environ(),
		"PATH="+shadow+":"+os.Getenv("PATH"),
		"BILLET_TEST_MKFS_LOG="+mkfsLog,
	)
	output, err := run.CombinedOutput()
	if err == nil {
		t.Fatal("a blkid I/O error was accepted as a blank cache volume")
	}
	if !strings.Contains(string(output), "signature could not be read") {
		t.Fatalf("blkid failure returned the wrong reason:\n%s", output)
	}
	if _, err := os.Stat(mkfsLog); !os.IsNotExist(err) {
		t.Fatalf("mkfs was called after blkid failed: %v", err)
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
	if err := os.WriteFile(script, []byte(DockerCacheScript), 0o755); err != nil {
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
	output, err := run.CombinedOutput()
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

	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write %s stub: %v", name, err)
	}
}
