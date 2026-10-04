package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// drainOnStopName is the marker in the server's state directory that makes this
// host's next stop drain rather than hand over (#365). The package's pre-removal
// script writes it, because a removal leaves no control plane on this host to hand
// over to, and sealing the deployment instead would stop every other controller
// from admitting work; deploy/scripts/preremove.sh names the same file.
const drainOnStopName = "drain-on-stop"

// stopIsFinal reports whether this host's stop must drain. A marker it cannot
// check counts as present: draining is the stop billet always had, and a handoff
// is the one that needs permission.
func stopIsFinal(stateDir string) func() bool {
	path := filepath.Join(stateDir, drainOnStopName)

	return func() bool {
		_, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return false
		}
		if err != nil {
			slog.Warn("could not check whether this stop must drain, so it drains",
				"marker", path, "error", err)
		}

		return true
	}
}

// clearStopMarker removes a marker a removal left behind that never completed, so
// a reinstalled server hands over on its next restart. Called at startup, before
// any stop can be asked; a marker that cannot be removed leaves the next stop a
// drain, which is the safe direction, and says so.
func clearStopMarker(stateDir string) {
	path := filepath.Join(stateDir, drainOnStopName)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove a drain-on-stop marker left by an unfinished package "+
			"removal; this server's next stop will drain rather than hand over",
			"marker", path, "error", err)
	}
}
