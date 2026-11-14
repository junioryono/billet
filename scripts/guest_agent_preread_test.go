package scripts_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// BILLET IS READ IN, IN THE BACKGROUND, AS SOON AS THE GUEST KNOWS A CACHE WILL RUN IT
// (#878): the Go, Bazel and Git caches each run billet as a helper, and a fresh EC2
// guest's first exec of it took 9.0 s against Bazel's 10 s wait. The block is executed
// with a reader that records what it was given and then holds until released, so the
// block returning while the reader still holds proves boot does not wait on the read.
func TestTheAgentReadsBilletInWhenACacheWillRunIt(t *testing.T) {
	t.Parallel()

	block := agentBlock(t, "BILLET_PREREAD")
	billet := filepath.Join(t.TempDir(), "billet")
	if err := forkSafeWriteFile(billet, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		endpoint string
		caches   string
		binary   string
		wantRead bool
	}{
		{name: "go", endpoint: "http://172.31.0.1:7718", caches: "go", binary: billet, wantRead: true},
		{name: "bazel among others", endpoint: "http://172.31.0.1:7718", caches: "docker,bazel,npm",
			binary: billet, wantRead: true},
		{name: "git", endpoint: "http://172.31.0.1:7718", caches: "git", binary: billet, wantRead: true},
		{name: "go test results alone", endpoint: "http://172.31.0.1:7718", caches: "go-test-results",
			binary: billet},
		{name: "no cache runs billet", endpoint: "http://172.31.0.1:7718", caches: "docker,npm", binary: billet},
		{name: "no cache session", caches: "go,bazel", binary: billet},
		{name: "an image without billet", endpoint: "http://172.31.0.1:7718", caches: "go",
			binary: filepath.Join(t.TempDir(), "absent")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			marker, release := filepath.Join(dir, "read"), filepath.Join(dir, "release")
			reader := filepath.Join(dir, "reader")
			// PUBLISHED WHOLE, by a rename, and then held until released.
			if err := forkSafeWriteFile(reader, []byte("#!/bin/sh\n"+
				"printf '%s' \"$1\" >"+shellQuote(marker+".new")+" && mv "+shellQuote(marker+".new")+" "+
				shellQuote(marker)+"\n"+
				"while [ ! -e "+shellQuote(release)+" ]; do sleep 0.05; done\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			// A READ NOBODY WANTS IS RELEASED BEFORE IT STARTS, so one started wrongly
			// publishes its marker and ends, and the group's end is what the absence is
			// judged after.
			if !tc.wantRead {
				if err := forkSafeWriteFile(release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			script := strings.Join([]string{
				"set -euo pipefail",
				"cache_endpoint=" + shellQuote(tc.endpoint),
				"guest_caches=" + shellQuote(tc.caches),
				"GUEST_BILLET=" + shellQuote(tc.binary),
				"GUEST_PREREAD=" + shellQuote(reader),
				block,
				"echo done",
			}, "\n")
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			// ITS OWN PROCESS GROUP, which a background read stays in (bash runs no job
			// control), so the test knows when everything the block started has ended.
			cmd := exec.CommandContext(ctx, "bash", "-c", script)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
			var output strings.Builder
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			group := cmd.Process.Pid
			// Registered after t.TempDir's, so it runs first: nothing the block started
			// outlives the test or the directory it writes in.
			t.Cleanup(func() {
				if groupGone(group, 0) {
					return
				}
				if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("end the block's processes: %v", err)
				}
				if !groupGone(group, 10*time.Second) {
					t.Errorf("the block's process group %d outlived its kill", group)
				}
			})
			err := cmd.Wait()
			if err != nil || strings.TrimSpace(output.String()) != "done" {
				t.Fatalf("the block did not return while its read was held: %v\n%s", err, output.String())
			}

			if tc.wantRead {
				deadline := time.Now().Add(5 * time.Second)
				var read []byte
				for time.Now().Before(deadline) {
					if read, err = os.ReadFile(marker); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if string(read) != tc.binary {
					t.Errorf("billet was not read in: read %q (%v), want %s", read, err, tc.binary)
				}
				if groupGone(group, 0) {
					t.Errorf("the read ended before it was released")
				}
				if err := os.WriteFile(release, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !groupGone(group, 10*time.Second) {
				t.Fatalf("what the block started has not ended")
			}
			if read, err := os.ReadFile(marker); !tc.wantRead && err == nil {
				t.Errorf("billet was read in though no cache runs it: %q", read)
			}
		})
	}
}

// groupGone reports whether no process is left in the group within the time given.
func groupGone(group int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}
