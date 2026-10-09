package uplink

import (
	"errors"
	"os"
)

// ErrReplaced is how a shaper ends when the billet it was started from has
// been replaced: an error, so the unit's Restart=on-failure starts the new one.
var ErrReplaced = errors.New("the installed billet was replaced; restarting onto it")

// executable is the installed billet as this process found it at start.
type executable struct {
	path string
	info os.FileInfo
}

// currentExecutable records the file this process runs and the path it was
// installed at, or nothing when it cannot say, in which case it is never
// thought replaced.
//
// THE RUNNING FILE FROM /proc/self/exe, NOT FROM THE PATH. A shaper that starts
// while an upgrade has the path empty, or after the new file is already there,
// would otherwise record the wrong file, or none, and never notice it runs the
// old release (internal/lifeops/exe_linux.go has the same distinction). The link
// resolves to the running inode whether or not its name still exists; where
// there is no /proc, the path is all there is.
func currentExecutable() executable {
	path, err := os.Executable()
	if err != nil {
		return executable{}
	}

	info, err := os.Stat("/proc/self/exe")
	if err != nil {
		if info, err = os.Stat(path); err != nil {
			return executable{}
		}
	}

	return executable{path: path, info: info}
}

// replaced reports whether another file now stands at the executable's path.
// EVERY UPGRADE PATH REPLACES IT, the host role's transaction, `billet
// host-upgrade` under a rollout and a package alike, and none of them knows the
// shaper is running: a shaper that noticed nothing would run the old release
// until the next reboot. A path with nothing at it is a transaction mid-swap,
// not a replacement, and the shaper goes on until the new file appears.
func (e executable) replaced() bool {
	if e.info == nil {
		return false
	}

	now, err := os.Stat(e.path)
	if err != nil {
		return false
	}

	return !os.SameFile(e.info, now)
}
