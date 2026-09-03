package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheWorkspaceIsNeverDeletedThroughALiveMount is the guard that stops a
// killed build from making the next one destructive.
//
// THE BUILD WRITES THROUGH A MOUNTPOINT and the next run's first act is
// `rm -rf "$WORK"`. A build that died between mount and unmount therefore leaves
// the next one recursively deleting the CONTENTS OF A LIVE FILESYSTEM rather than
// the directory it thinks it is clearing -- and the workspace-ownership marker
// does not help, because the marker is exactly what a previous run leaves behind.
//
// FAKE mountpoint AND umount, because the property under test is the ORDERING and
// the refusal, not the kernel's behaviour. Driving it with real mounts would need
// root and Linux and would test the same two branches.
func TestTheWorkspaceIsNeverDeletedThroughALiveMount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		mounted     bool
		umountFails bool
		wantSuccess bool
		wantUmount  bool
	}{
		{name: "nothing mounted", mounted: false, wantSuccess: true},
		{name: "a stale mount is cleared", mounted: true, wantSuccess: true, wantUmount: true},
		// THE IMPORTANT ONE. If the mount cannot be cleared, the build must STOP:
		// the caller's very next statement is a recursive delete.
		{
			name: "an unclearable mount stops the build", mounted: true,
			umountFails: true, wantSuccess: false, wantUmount: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tools := t.TempDir()
			calls := filepath.Join(tools, "calls.log")

			mountpointExit := "1"
			if tc.mounted {
				mountpointExit = "0"
			}

			writeExecutable(t, filepath.Join(tools, "mountpoint"),
				"#!/bin/sh\nexit "+mountpointExit+"\n")

			umountExit := "0"
			if tc.umountFails {
				umountExit = "1"
			}

			writeExecutable(t, filepath.Join(tools, "umount"),
				"#!/bin/sh\nprintf 'umount %s\\n' \"$*\" >> \""+calls+"\"\nexit "+umountExit+"\n")

			script := "#!/usr/bin/env bash\nset -euo pipefail\n" +
				guestImageFunction(t, "clear_stale_mount") + "\n" +
				"clear_stale_mount /tmp/does-not-matter\n" +
				"echo REACHED_THE_DELETE\n"

			path := filepath.Join(t.TempDir(), "run.sh")
			if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
				t.Fatalf("write the harness: %v", err)
			}

			cmd := exec.CommandContext(t.Context(), "bash", path)
			cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))

			out, err := cmd.CombinedOutput()

			if got := err == nil; got != tc.wantSuccess {
				t.Fatalf("clear_stale_mount success = %v, want %v\n%s", got, tc.wantSuccess, out)
			}

			// ASSERTED ON THE THING THAT MATTERS: whether the caller went on to the
			// recursive delete. A test that only checked the exit status would pass
			// against a version that printed an error and continued.
			reached := strings.Contains(string(out), "REACHED_THE_DELETE")
			if reached != tc.wantSuccess {
				t.Errorf("the build reached the recursive delete = %v, want %v; a mount that "+
					"could not be cleared must stop before it\n%s", reached, tc.wantSuccess, out)
			}

			if got := strings.Contains(readCalls(t, calls), "umount"); got != tc.wantUmount {
				t.Errorf("umount attempted = %v, want %v", got, tc.wantUmount)
			}
		})
	}
}

// TestTheUnmountTrapIsANoOpWhenNothingIsMounted: the trap runs on EVERY exit path
// including the ones before anything is mounted, so it must not fail a build that
// succeeded.
func TestTheUnmountTrapIsANoOpWhenNothingIsMounted(t *testing.T) {
	t.Parallel()

	tools := t.TempDir()

	// NEITHER TOOL SHOULD BE REACHED. If the trap consults them with an empty
	// MOUNTED_ROOTFS it is doing work it cannot justify, and on a build that never
	// mounted anything that work can only be wrong.
	for _, name := range []string{"mountpoint", "umount", "sync"} {
		writeExecutable(t, filepath.Join(tools, name),
			"#!/bin/sh\necho \"CALLED "+name+"\" >&2\nexit 0\n")
	}

	script := "#!/usr/bin/env bash\nset -euo pipefail\nMOUNTED_ROOTFS=\"\"\n" +
		guestImageFunction(t, "unmount_rootfs") + "\n" +
		"unmount_rootfs\necho CLEAN\n"

	path := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the harness: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), "bash", path)
	cmd.Env = append(os.Environ(), "PATH="+tools+":"+os.Getenv("PATH"))

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the trap failed with nothing mounted: %v\n%s", err, out)
	}

	if strings.Contains(string(out), "CALLED") {
		t.Errorf("the trap ran unmount work with nothing mounted:\n%s", out)
	}

	if !strings.Contains(string(out), "CLEAN") {
		t.Errorf("the harness did not finish:\n%s", out)
	}
}
