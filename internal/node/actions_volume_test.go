package node

import (
	"os"
	"path/filepath"
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
	nodiscard := false
	for i := range len(args) - 1 {
		if args[i] == "-E" && strings.Contains(args[i+1], "nodiscard") {
			nodiscard = true
		}
	}
	if !nodiscard || args[len(args)-1] != "/dev/rbd7" {
		t.Fatalf("mkfs.ext4 ran with %q, want -E nodiscard and the device last", args)
	}
}
