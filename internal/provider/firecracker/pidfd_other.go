//go:build !linux

package firecracker

import (
	"errors"
	"syscall"
)

// This backend runs on linux and only has to COMPILE elsewhere, because the
// cross-build matrix includes darwin and because a developer on a laptop runs the
// package's tests. There is no portable equivalent of a pidfd, and the point of the
// linux version is that it is safe in a way signalling a raw pid is not — so the
// stand-in refuses rather than approximating it.
//
// Nothing reaches this in practice: the preflight requires /dev/kvm and /proc, and
// every test that exercises process identity skips where /proc is absent.

type vmmHandle struct{}

func openVMM(_ int) (vmmHandle, bool, error) {
	return vmmHandle{}, false, errors.New("firecracker: microVMs are a linux backend, and this " +
		"host cannot hold a reference to a process safely enough to signal one")
}

func (vmmHandle) signal(_ syscall.Signal) error {
	return errors.New("firecracker: microVMs are a linux backend")
}

func (vmmHandle) close() {}
