package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// THE INSTALLED BINARY KEEPS ITS MODE UNDER THE UPDATER'S UMASK.
//
// billet-upgrade.service runs as root with UMask=0077, and the write that stages
// the candidate is subject to it: a 0755 candidate landed as 0700 root:root, and
// billet-server, which runs as the service account, could not execute the binary
// the transaction had just installed for it, so every timer-driven upgrade of a
// control plane ended at "the services did not start" (2026-09-05). A rollback
// restores the previous binary through the same copy and left it the same way.
//
// NOT PARALLEL: the umask is process-wide, and nothing else in this package may
// create a file while it is 0077.
func TestTheInstalledBinaryKeepsItsModeUnderTheUpdatersUmask(t *testing.T) {
	previous := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previous) })

	dir := t.TempDir()
	from := filepath.Join(dir, "billet.candidate")
	to := filepath.Join(dir, "billet")

	if err := os.WriteFile(from, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// THE CANDIDATE IS 0755 THE WAY THE STAGING STEP LEAVES IT: by an explicit
	// chmod, which the umask does not touch.
	if err := os.Chmod(from, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyFile(from, to); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	info, err := os.Stat(to)
	if err != nil {
		t.Fatal(err)
	}

	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("the installed binary is %04o under umask 0077; want 0755, or the service "+
			"account cannot execute it", got)
	}
}
