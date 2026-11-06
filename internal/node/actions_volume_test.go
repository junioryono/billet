package node

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A NEW VOLUME IS FORMATTED WITHOUT A DISCARD PASS. It is a thin rbd image Create
// has just made, and on 2026-10-03 mke2fs's default discard of every block was
// killed under load, leaving a session's cache unmounted for the whole build.
//
// NOT PARALLEL: the fake mkfs.ext4 reaches MountNew through PATH.
func TestANewCacheVolumeIsFormattedWithoutADiscardPass(t *testing.T) {
	bin := t.TempDir()
	argv := filepath.Join(t.TempDir(), "argv")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argv + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "mkfs.ext4"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := hostActionsVolumeManager{}.MountNew(t.Context(), "/dev/rbd7", filepath.Join(t.TempDir(), "mnt"))
	if err == nil {
		t.Fatal("MountNew succeeded although the fake mkfs.ext4 failed")
	}

	recorded, readErr := os.ReadFile(argv)
	if readErr != nil {
		t.Fatalf("the fake mkfs.ext4 was never run: %v (MountNew: %v)", readErr, err)
	}

	args := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	want := []string{"-F", "-m", "0", "-E", "nodiscard", "/dev/rbd7"}
	if !slices.Equal(args, want) {
		t.Fatalf("mkfs.ext4 ran with %q, want exactly %q", args, want)
	}
}

// A MOUNT WHOSE COMMAND FAILED IS UNDONE IF THE KERNEL HOLDS IT. A mount killed by
// its context can have finished mounting: on 2026-11-06 one left a Go cache
// volume mounted that the node believed was not, and its discard could not unmap
// the device for hours. The kernel is asked, and only what it holds is unmounted;
// a look that fails says the mount may remain rather than calling it gone.
//
// NOT PARALLEL: the fake mount, mountpoint and umount reach the manager through PATH.
func TestAFailedMountIsUndoneWhenTheKernelHoldsIt(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mounted     bool
		look        string
		wantUnmount bool
		wantKept    bool
		wantErr     string
	}{
		{name: "killed after the kernel mounted", mounted: true, look: "ok",
			wantUnmount: true, wantErr: "signal: killed"},
		{name: "failed without mounting", mounted: false, look: "ok",
			wantErr: "mount Actions cache volume"},
		{name: "could not tell", mounted: true, look: "fail",
			wantKept: true, wantErr: "may still be mounted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, state := t.TempDir(), t.TempDir()
			mountedFlag := filepath.Join(state, "mounted")
			umounted := filepath.Join(state, "umounted")
			if tc.mounted {
				if err := os.WriteFile(mountedFlag, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			mountpoint := "#!/bin/sh\n"
			if tc.look == "fail" {
				mountpoint += "exit 1\n"
			} else {
				mountpoint += "[ -e '" + mountedFlag + "' ] && exit 0\nexit 32\n"
			}
			for name, body := range map[string]string{
				// Killed the way a cancelled exec.CommandContext kills it.
				"mount":      "#!/bin/sh\nkill -9 $$\n",
				"mountpoint": mountpoint,
				"umount":     "#!/bin/sh\nrm -f '" + mountedFlag + "'\n: > '" + umounted + "'\n",
			} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			target := filepath.Join(t.TempDir(), "go")
			err := hostActionsVolumeManager{}.MountWritable(t.Context(), "/dev/rbd102", target)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("MountWritable = %v, want an error containing %q", err, tc.wantErr)
			}
			_, unmountErr := os.Stat(umounted)
			if ran := unmountErr == nil; ran != tc.wantUnmount {
				t.Fatalf("umount ran = %t, want %t", ran, tc.wantUnmount)
			}
			_, statErr := os.Stat(target)
			if kept := statErr == nil; kept != tc.wantKept {
				t.Fatalf("mount point kept = %t (%v), want %t", kept, statErr, tc.wantKept)
			}
		})
	}
}

// AN UNMOUNTED MOUNT POINT IS REMOVED, AND A FAILURE TO LOOK IS NOT UNMOUNTED.
// util-linux's mountpoint exits 32 for "not a mount point" and 1 for an error;
// reading 1 as unmounted left 159 closed sessions on the reference deployment
// unable to finish (2026-10-03).
//
// NOT PARALLEL: the fake mountpoint reaches Unmount through PATH.
func TestUnmountReadsMountpointsExitStatus(t *testing.T) {
	for _, tc := range []struct {
		status  string
		wantErr bool
	}{
		{status: "32", wantErr: false},
		{status: "1", wantErr: true},
	} {
		t.Run("exit "+tc.status, func(t *testing.T) {
			bin := t.TempDir()
			fake := "#!/bin/sh\nexit " + tc.status + "\n"
			if err := os.WriteFile(filepath.Join(bin, "mountpoint"), []byte(fake), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

			target := filepath.Join(t.TempDir(), "mnt")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}

			err := hostActionsVolumeManager{}.Unmount(t.Context(), target)
			_, statErr := os.Stat(target)
			if tc.wantErr {
				if err == nil {
					t.Fatal("Unmount succeeded although mountpoint could not look")
				}
				if statErr != nil {
					t.Fatalf("Unmount removed a mount point it could not inspect: %v", statErr)
				}

				return
			}
			if err != nil {
				t.Fatalf("Unmount: %v", err)
			}
			if !os.IsNotExist(statErr) {
				t.Fatalf("the unmounted mount point was left behind: %v", statErr)
			}
		})
	}
}
