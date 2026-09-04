package main

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// hostPaths is where a host keeps the binary the services run and the
// transaction's own bookkeeping.
//
// ONE ANSWER PER PLATFORM, CHOSEN ONCE. The packaged units run /usr/bin/billet
// and the launch agents run /usr/local/bin/billet; the recovery journal, the
// claim, the decision mark and the transaction lock live under a directory the
// updater can write as the account it runs as — root on Linux, the operator on a
// Mac. Two files each spelling one of these is how an updater replaces a binary
// the service does not execute, and the plists and units are the authority this
// has to agree with, so it is keyed on the platform the same way
// initconfig.ServiceConfigPathFor is.
type hostPaths struct {
	// binary is the executable the services run, and what the transaction
	// hides, replaces and restores.
	binary string
	// upgradeRoot holds the recovery journals, the active claim, the decision
	// mark and the transaction lock.
	upgradeRoot string
}

// hostPathsFor answers for a platform.
func hostPathsFor(goos string) hostPaths {
	if goos == "darwin" {
		return hostPaths{
			binary:      "/usr/local/bin/billet",
			upgradeRoot: "/usr/local/var/lib/billet/upgrades",
		}
	}

	return hostPaths{
		binary:      "/usr/bin/billet",
		upgradeRoot: "/var/lib/billet/upgrades",
	}
}

// binaryDir is the directory the binary is renamed into, which is what has to
// be writable for a replacement to land.
func (p hostPaths) binaryDir() string { return filepath.Dir(p.binary) }

// checkBinaryDirWritable refuses an upgrade that could not land, before anything
// is drained.
//
// THE ONE PRIVILEGED PATH ON A MAC. /usr/local/bin is root-owned on a stock
// machine and install.sh writes the binary there with sudo, but the updater runs
// as the operator's launch agent does — as the operator — and replacing the
// binary is a rename into that directory. Discovering that after the node has
// drained and the old binary is hidden would be a rollback for a fact that was
// knowable before the ack. On Linux the updater is root and this never refuses.
//
// unix.Access IS APPROXIMATE IN THE PERMISSIVE DIRECTION, which is the right
// direction for a preflight: a refusal here is certain, and a pass that turns
// out wrong is caught by the rename itself and rolled back.
func checkBinaryDirWritable(paths hostPaths) error {
	dir := paths.binaryDir()

	if err := unix.Access(dir, unix.W_OK); err != nil {
		return fmt.Errorf("%s is not writable by this account, so a replacement of %s could "+
			"not land; give it the directory with `sudo chown $(id -un) %s` (the launch "+
			"agents run as this account, and so does the updater they start): %w",
			dir, paths.binary, dir, err)
	}

	return nil
}

// installedBinary is the executable the services on this host run.
//
// A VAR RATHER THAN A CONST, for the reason upgradeRoot is: a test owns what it
// replaces. Both are read from hostPathsFor(hostOS) so the two cannot name
// different platforms.
var installedBinary = hostPathsFor(hostOS).binary
