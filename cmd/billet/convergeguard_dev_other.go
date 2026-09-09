//go:build !linux

package main

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// devOf widens the device number, which is narrower than 64 bits here.
func devOf(st *syscall.Stat_t) uint64 { return uint64(st.Dev) }

// modeOf widens a stat's mode bits, which are 16 bits here.
func modeOf(st *unix.Stat_t) uint32 { return uint32(st.Mode) }

// openIdentityAt opens an entry non-blocking relative to the directory, the
// nearest thing to an identity-only open this platform has (a device is
// opened and then refused by regularfile.Reopen, as that package says).
func openIdentityAt(dir *os.File, name string) (*os.File, error) {
	return openIdentityFD(int(dir.Fd()), name, filepath.Join(dir.Name(), name))
}

// openIdentityFD is openIdentityAt over a raw directory descriptor the caller
// keeps, named for the *os.File it returns.
func openIdentityFD(dirFD int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	return os.NewFile(uintptr(fd), path), nil
}
