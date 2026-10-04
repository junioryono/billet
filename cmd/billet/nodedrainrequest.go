package main

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// nodeDrainRequestPath is where a stop that must be a drain names the node
// process it is about to stop (#374). The host role's request-node-drain.yml
// and the package's preremove.sh write it; a test holds all three spellings
// equal.
const nodeDrainRequestPath = "/run/billet-node-drain"

// nodeDrainRequested reports whether the stop now under way must drain even
// though node.stop says handoff.
//
// A STOP THAT REMOVES THE NODE OR ITS GUESTS' NETWORKING MUST NOT LEAVE GUESTS
// BEHIND, and says so before it stops the unit by writing this process's pid to
// nodeDrainRequestPath. Written before the stop is sent, so there is no ordering
// to lose; bound to the pid, so a request left by an earlier stop never applies
// to a later process; read from a file, not the config, so it asks the process
// that is running whatever a converge is about to install. An answer that
// cannot be read is a drain: the stop then waits, which is slow, where a
// wrong handoff would strand guests on a host being taken apart.
func nodeDrainRequested() bool {
	return drainRequestedFor(nodeDrainRequestPath, os.Getpid())
}

func drainRequestedFor(path string, pid int) bool {
	body, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		slog.Default().Warn("could not read whether this stop must drain; draining rather than "+
			"handing over", "path", path, "error", err)

		return true
	}

	return strings.TrimSpace(string(body)) == strconv.Itoa(pid)
}
