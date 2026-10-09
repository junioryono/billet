//go:build unix

package uplink

import (
	"os"
	"syscall"
)

// flock takes an exclusive, non-blocking lock on f.
func flock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
