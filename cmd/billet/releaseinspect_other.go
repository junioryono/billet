//go:build !linux

package main

import "os"

// openInRoot resolves path under rootLink the way RESOLVE_IN_ROOT does, in user
// space (resolveInRoot). This build never inspects a real service (the services
// section is unknown off Linux); it exists so the fixture-driven tests exercise
// the same contract the Linux build gets from the kernel.
func openInRoot(rootLink, path string) (*os.File, error) {
	resolved, err := resolveInRoot(rootLink, path)
	if err != nil {
		return nil, err
	}
	return os.Open(resolved)
}
