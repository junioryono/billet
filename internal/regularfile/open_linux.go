//go:build linux

package regularfile

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// openForIdentity names the inode without opening it: an O_PATH descriptor
// waits on no FIFO and performs no device open, and is what the fstat below
// looks at.
func openForIdentity(path string, opts Options) (*os.File, error) {
	flags := unix.O_PATH | unix.O_CLOEXEC
	if opts.NoFollow {
		flags |= unix.O_NOFOLLOW
	}
	fd, err := unix.Open(path, flags, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// reopen reads THE INODE ALREADY HELD, through /proc/self/fd, so no pathname is
// resolved a second time and a replacement between the identity open and this
// one cannot be read for the original. An O_PATH descriptor of a symlink taken
// with O_NOFOLLOW is the link itself and fails the regular-file rule.
func reopen(id *os.File) (*os.File, os.FileInfo, error) {
	info, err := requireRegular(id)
	if err != nil {
		return nil, nil, err
	}
	// The name is this process's own descriptor number and nothing from the
	// host: a reopen of an inode already held, never a path resolved again.
	f, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", id.Fd()))
	if err != nil {
		return nil, nil, &os.PathError{Op: "reopen", Path: id.Name(), Err: err}
	}
	return f, info, nil
}
