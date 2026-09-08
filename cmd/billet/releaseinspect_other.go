//go:build !linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// openInRoot resolves path under rootLink the way RESOLVE_IN_ROOT does, in user
// space (resolveInRoot), and opens the result for its identity: read-only and
// non-blocking, so a FIFO does not hold the inspector. This build never inspects
// a real service (the services section is unknown off Linux); it exists so the
// fixture-driven tests exercise the same contract the Linux build gets from the
// kernel.
func openInRoot(rootLink, path string) (*os.File, error) {
	resolved, err := resolveInRoot(rootLink, path)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// reopenForReading is the fixture's stand-in for the Linux reopen through
// /proc/self/fd: the same regular-file rule, and a second open of the resolved
// name, which is racy where the kernel's is not and serves no production report.
func reopenForReading(f *os.File) (*os.File, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file (%s)", info.Mode().Type())
	}
	return os.Open(f.Name()) //nolint:gosec // the name is the descriptor's own, resolved under the fixture root and already opened for identity; this build inspects no real service
}
