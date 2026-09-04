//go:build unix

package github

import (
	"os"
	"syscall"
)

// OpenForInspection opens a path without blocking on it.
//
// A plain os.Open of a FIFO blocks until a writer appears, so pointing
// github.private_key_path at one made `billet check` hang forever rather than
// report anything. O_NONBLOCK returns immediately; the caller then rejects the
// FIFO through the regular-file check, which is the diagnosis the operator
// needs.
//
// Opening and THEN inspecting is what makes the check honest: the size, mode
// and contents all describe the same descriptor, so nothing can be swapped
// between the check and the read.
func OpenForInspection(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
