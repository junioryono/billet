//go:build linux

package firecracker

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// vmmHandle is a reference to one process that CANNOT come to mean another one.
//
// THE PID ITSELF IS NOT SAFE TO SIGNAL, and this is the last place that mattered.
// Everything else in this package already refuses to act on a pid it cannot tie to a
// jail — but "verify, then signal" is two operations, and between them the VMM can
// exit and the kernel can give its number to something else. The signal then lands on
// an unrelated process, sent as root, on evidence that was true when it was gathered.
//
// A pidfd closes that gap by construction: it refers to the process that was open, not
// to the number. If that process is gone, sending through the handle fails with ESRCH
// rather than reaching whatever now holds the pid. So the check and the signal are
// about the same process even though they are not atomic.
//
// The ordering that makes this work is subtle and worth stating: the handle is opened
// FIRST, and the /proc check happens after. If the pid were recycled in between, the
// check would read the NEW process's command line and could approve — but the signal
// still goes through the handle to the OLD one, which is dead, so the result is a
// harmless ESRCH rather than a signal to a stranger. Being wrong in that direction is
// the entire design goal.
type vmmHandle struct{ fd int }

// openVMM takes a reference to a running process.
//
// A process that is already gone is reported as such rather than as an error: it is
// the ordinary case teardown runs into constantly, and it means there is nothing left
// to stop.
func openVMM(pid int) (vmmHandle, bool, error) {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return vmmHandle{}, false, nil
		}

		// ENOSYS on a kernel older than 5.3. billet's reference platform is far past
		// that, and the honest answer to "this host cannot hold a process reference"
		// is to stop rather than to fall back to signalling a number — the fallback
		// is exactly the unsafe operation this type exists to remove.
		return vmmHandle{}, false, fmt.Errorf("firecracker: take a reference to pid %d: %w", pid, err)
	}

	return vmmHandle{fd: fd}, true, nil
}

// signal sends to the process this handle was opened on, and nothing else.
func (h vmmHandle) signal(sig syscall.Signal) error {
	if err := unix.PidfdSendSignal(h.fd, sig, nil, 0); err != nil {
		if errors.Is(err, unix.ESRCH) {
			// It exited between the handle being taken and the signal being sent,
			// which is the outcome teardown wanted anyway.
			return nil
		}

		return fmt.Errorf("firecracker: signal the microVM's vmm: %w", err)
	}

	return nil
}

func (h vmmHandle) close() {
	_ = unix.Close(h.fd)
}
