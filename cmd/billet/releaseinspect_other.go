//go:build !linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// openInRoot resolves path under rootLink the way RESOLVE_IN_ROOT does, in user
// space (resolveInRoot), and opens the result for its identity: read-only and
// non-blocking, so a FIFO does not hold the inspector, and nothing is read
// through the descriptor until reopenForReading has found a regular file. This
// build never inspects a real service (the services section is unknown off
// Linux); it exists so the fixture-driven tests exercise the same contract the
// Linux build gets from the kernel.
func openInRoot(rootLink, path string) (*os.File, error) {
	resolved, err := resolveInRoot(rootLink, path)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// reopenForReading applies the regular-file rule and returns a second
// descriptor on THE SAME OPEN FILE, by duplicating it, so no pathname is
// resolved a second time and a replacement between the two cannot be read for
// the original; a regular file reads normally through a non-blocking
// descriptor.
func reopenForReading(f *os.File) (*os.File, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: f.Name(), Err: fmt.Errorf("not a regular file (%s)", info.Mode().Type())}
	}
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return nil, &os.PathError{Op: "dup", Path: f.Name(), Err: err}
	}
	syscall.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), f.Name()), nil
}
