//go:build linux

package lifeops

import (
	"fmt"
	"io/fs"
	"os"
)

// selfExe stats the executable THIS process is running.
//
// /proc/self/exe, NOT os.Executable(). Go's Linux implementation reads that
// same link and then strips the kernel's " (deleted)" suffix before returning a
// pathname — so after a binary is replaced in place, stat-ing the returned path
// stats the REPLACEMENT while this process is still running the old inode. The
// link itself resolves to the running inode whether or not its name still
// exists, which is the only thing an identity comparison can be built on.
func selfExe() (fs.FileInfo, error) {
	return os.Stat("/proc/self/exe")
}

// processExe stats the executable a RUNNING process is executing.
//
// This is the judgment that survives a replaced binary: a service started
// before the replacement holds the old inode, and nothing about the unit file
// or the current /usr/bin/billet says so.
//
// Permission is expected to fail for an unprivileged caller inspecting a root
// service. That is uncertainty, not a negative, and callers must keep it so.
func processExe(pid int) (fs.FileInfo, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("no main pid")
	}

	return os.Stat(fmt.Sprintf("/proc/%d/exe", pid))
}
