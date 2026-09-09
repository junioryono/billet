package main

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// devOf is the device number as the hook records it; already 64 bits here.
func devOf(st *syscall.Stat_t) uint64 { return st.Dev }

// modeOf is a stat's mode bits as the classifier reads them; already 32 bits
// here.
func modeOf(st *unix.Stat_t) uint32 { return st.Mode }

// openIdentityAt names an entry without opening it: an O_PATH descriptor
// relative to the directory, which invokes no driver whatever the entry is.
func openIdentityAt(dir *os.File, name string) (*os.File, error) {
	return openIdentityFD(int(dir.Fd()), name, filepath.Join(dir.Name(), name))
}

// openIdentityFD is openIdentityAt over a raw directory descriptor the caller
// keeps, named for the *os.File it returns.
func openIdentityFD(dirFD int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), path), nil
}
