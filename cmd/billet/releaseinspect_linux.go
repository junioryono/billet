//go:build linux

package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// openInRoot opens an absolute path AS A PROCESS WHOSE ROOT IS rootLink SEES IT.
// A plain open of "<root>/<path>" is not that: the kernel resolves an absolute
// symlink met on the way against the caller's root, so a config that is a
// symlink into a directory the service has bind-mounted differently would be
// opened in the inspector's namespace and compare equal to a file the service
// never reads. openat2 with RESOLVE_IN_ROOT scopes every symlink and every ".."
// to the directory descriptor, which is the process's root in its own mount
// namespace when rootLink is /proc/<pid>/root; RESOLVE_NO_MAGICLINKS refuses a
// second procfs link on the way. A kernel without openat2 (before 5.6) answers
// ENOSYS, which the caller reports as could-not-tell.
func openInRoot(rootLink, path string) (*os.File, error) {
	rootFD, err := unix.Open(rootLink, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open the process root %s: %w", rootLink, err)
	}
	defer unix.Close(rootFD)
	how := unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOCTTY,
		Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS,
	}
	fd, err := unix.Openat2(rootFD, strings.TrimPrefix(path, "/"), &how)
	if err != nil {
		return nil, fmt.Errorf("open %s under the process root %s: %w", path, rootLink, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}
