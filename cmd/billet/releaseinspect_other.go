//go:build !linux

package main

import (
	"os"
	"syscall"
)

// openInRoot resolves path under rootLink the way RESOLVE_IN_ROOT does, in user
// space (resolveInRoot), and opens the result for its identity: read-only and
// non-blocking, so a FIFO does not hold the inspector, and nothing is read
// through the descriptor until reopenForReading (regularfile.Reopen) has found a
// regular file. This build never inspects a real service (the services section
// is unknown off Linux); it exists so the fixture-driven tests exercise the same
// contract the Linux build gets from the kernel.
func openInRoot(rootLink, path string) (*os.File, error) {
	resolved, err := resolveInRoot(rootLink, path)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
