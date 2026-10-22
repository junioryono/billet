//go:build !linux

package regularfile

import (
	"errors"
	"os"
	"syscall"
)

// mountOf has only the device to go on: this platform reports no mount
// identity, and has no single-file bind mount for one to catch.
func mountOf(f *os.File) (Mount, error) {
	info, err := f.Stat()
	if err != nil {
		return Mount{}, err
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Mount{}, errors.New("the platform reported no device")
	}

	return Mount{Dev: uint64(st.Dev)}, nil
}
