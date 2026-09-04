package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE PATHS AGREE WITH WHAT THE SERVICES RUN. The packaged units execute
// /usr/bin/billet and the launch agents /usr/local/bin/billet; an updater that
// replaced the other one would leave a service running the release it was
// meant to replace, with every surface reading upgraded.
func TestHostPathsNameWhatTheServicesRun(t *testing.T) {
	t.Parallel()

	linux := hostPathsFor("linux")
	if linux.binary != "/usr/bin/billet" || linux.upgradeRoot != "/var/lib/billet/upgrades" {
		t.Errorf("linux paths = %+v", linux)
	}

	mac := hostPathsFor("darwin")
	if mac.binary != "/usr/local/bin/billet" ||
		mac.upgradeRoot != "/usr/local/var/lib/billet/upgrades" {
		t.Errorf("darwin paths = %+v", mac)
	}

	if !strings.HasPrefix(mac.upgradeRoot, "/usr/local/var/lib/billet/") {
		t.Errorf("the Mac upgrade root %s is outside the tree the Mac setup creates for the "+
			"operator", mac.upgradeRoot)
	}
}

// A BINARY DIRECTORY THIS ACCOUNT CANNOT WRITE IS REFUSED BEFORE ANYTHING
// DRAINS, with the command that fixes it.
func TestAnUnwritableBinaryDirectoryIsRefusedByName(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root can write anywhere, so nothing here can be refused")
	}

	dir := t.TempDir()

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("give the temporary directory back its mode: %v", err)
		}
	})

	paths := hostPaths{binary: filepath.Join(dir, "billet")}

	err := checkBinaryDirWritable(paths)
	if err == nil {
		t.Fatal("an unwritable binary directory was accepted")
	}

	if !strings.Contains(err.Error(), "sudo chown") || !strings.Contains(err.Error(), dir) {
		t.Errorf("the refusal does not name the directory and the command: %v", err)
	}

	writable := hostPaths{binary: filepath.Join(t.TempDir(), "billet")}
	if err := checkBinaryDirWritable(writable); err != nil {
		t.Errorf("a writable binary directory was refused: %v", err)
	}
}
