package host

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
)

// nodeDrainRequestPath is where an operation that must not leave guests behind
// says so for as long as it runs (#374). The host role's request-node-drain.yml
// and release-node-drain.yml, the package's preremove.sh and `local down` and
// `local up` write or remove it; a test holds every spelling equal. /var/run
// rather than /run, so one spelling holds on Linux, where it is a link to /run,
// and on macOS.
const nodeDrainRequestPath = "/var/run/billet-node-drain"

// nodeDrainRequestFile is where `local down` and `local up` write and remove the
// request: nodeDrainRequestPath, which TestMain points into a temporary
// directory before any test runs, because a test host's /var/run is not ours.
var nodeDrainRequestFile = nodeDrainRequestPath

// nodeDrainRequested reports whether the stop now under way must drain even
// though node.stop says handoff.
//
// A STOP THAT REMOVES THE NODE OR ITS GUESTS' NETWORKING MUST NOT LEAVE GUESTS
// BEHIND. The operation that does it writes nodeDrainRequestPath before its
// first stop and removes it when it is done, so every node process that stops
// meanwhile drains, a replacement systemd started in between included; read from
// a file, not the config, so it binds the process that is running whatever a
// converge is about to install. A request left behind by an operation that
// failed only makes a later stop drain, which is slow and safe, and so does one
// that cannot be read.
func NodeDrainRequested() bool {
	return drainRequestedAt(nodeDrainRequestPath)
}

func drainRequestedAt(path string) bool {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		slog.Default().Warn("could not read whether this stop must drain; draining rather than "+
			"handing over", "path", path, "error", err)

		return true
	}

	slog.Default().Info("this stop drains: an operation that must not leave guests behind asked "+
		"for it", "path", path)

	return true
}

// requestNodeDrain writes the request before a stop that takes the node out of
// service, so a node set to hand over drains instead.
func requestNodeDrain(path string) error {
	if err := os.WriteFile(path, []byte("drain\n"), 0o644); err != nil { //nolint:gosec // root-owned and world-readable on purpose: the node reads it under its own unit's sandbox.
		return fmt.Errorf("ask the node to drain rather than hand over: %w", err)
	}

	return nil
}

// releaseNodeDrain removes the request once the node is meant to run again.
func releaseNodeDrain(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("withdraw the request that the node drain on its next stop: %w", err)
	}

	return nil
}
