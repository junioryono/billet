//go:build !linux

package guestagent

import (
	"fmt"
	"runtime"
)

// Supported refuses: the agent reads a Linux guest's /proc, and this is not Linux.
func Supported() error {
	return fmt.Errorf("billet guest monitor runs inside a Linux guest, and this is %s", runtime.GOOS)
}

// LowerPriority does nothing where the agent does not run.
func LowerPriority(int) error { return nil }
