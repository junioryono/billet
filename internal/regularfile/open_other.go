//go:build !linux

package regularfile

import (
	"os"
	"syscall"
)

// openForIdentity has no identity-only open to use: the file is opened read-only
// and NON-BLOCKING, which returns at once on a FIFO instead of waiting for a
// writer, and nothing is read through the descriptor until reopen has applied
// the regular-file rule. A device is opened by this call and then refused; that
// is the limit of what this platform offers, and the package comment says so.
func openForIdentity(path string, opts Options) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK | syscall.O_CLOEXEC
	if opts.NoFollow {
		flags |= syscall.O_NOFOLLOW
	}
	return os.OpenFile(path, flags, 0)
}

// reopen duplicates the descriptor after the regular-file rule, so the readable
// descriptor is THE SAME OPEN FILE and no pathname is resolved a second time; a
// regular file reads normally through a non-blocking descriptor.
func reopen(id *os.File) (*os.File, os.FileInfo, error) {
	info, err := requireRegular(id)
	if err != nil {
		return nil, nil, err
	}
	fd, err := syscall.Dup(int(id.Fd()))
	if err != nil {
		return nil, nil, &os.PathError{Op: "dup", Path: id.Name(), Err: err}
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), id.Name()), info, nil
}
