package firecracker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// pidIsVMM reports whether a process id is still this jail's VMM.
//
// THE ANSWER COMES FROM THE COMMAND LINE, which is the only thing that survives pid
// reuse. A pid file holds a number; the kernel gives that number to something else
// once the process ends, and this backend runs as root — so "the file says 4321" is
// not grounds to signal 4321. The jailer execs the VMM with `--id <jail id>`
// (measured), so the argument vector is proof.
//
// AN ERROR IS "COULD NOT TELL", AND IT IS NOT THE SAME AS FALSE. False means the
// process is gone or is something else, and teardown may go on to unmap the block
// device; an unreadable /proc means billet does not know, and unmapping a device a
// live VMM still holds is how a guest's filesystem is torn out mid-job. Only a
// definite absence answers false.
func pidIsVMM(pid int, jailID string) (bool, error) {
	raw, err := os.ReadFile(procCmdline(pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The process is gone. On Linux this is the ordinary answer for an
			// exited pid, and it is the one this function exists to give.
			return false, nil
		}

		if errors.Is(err, os.ErrPermission) {
			// A process billet may not inspect is not one it may signal either, and
			// it is emphatically not evidence that the VMM has exited.
			return false, fmt.Errorf("firecracker: cannot tell whether pid %d is still the "+
				"microVM %s: %w", pid, jailID, err)
		}

		return false, fmt.Errorf("firecracker: read the command line of pid %d: %w", pid, err)
	}

	// THE PAIR `--id <jail id>`, NOT THE ID ANYWHERE IN THE LINE. Matching a lone
	// argument would claim any process that happens to mention this lease — one of
	// billet's own subprocesses, a shell an operator left open — and what follows a
	// true answer here is a signal sent as root.
	//
	// ARGUMENTS ARE NUL-SEPARATED, so these are whole ones. A substring match over
	// the raw bytes would also match a jail id that merely CONTAINS this one, and
	// lease ids are fixed-length hex, so that is a prefix relationship rather than
	// an exotic one.
	args := bytes.Split(raw, []byte{0})

	for i, arg := range args {
		if string(arg) == "--id" && i+1 < len(args) && string(args[i+1]) == jailID {
			return true, nil
		}
	}

	return false, nil
}

// procCmdline is where a process's argument vector is readable.
//
// Linux only, which is where this backend runs — every other platform reads it as a
// path that does not exist, so pidIsVMM answers "gone" and Destroy signals nothing.
// That is the fail-closed direction: it leaves a jail to be cleaned up rather than
// signalling a pid on a system where billet cannot check what it points at.
func procCmdline(pid int) string {
	return "/proc/" + strconv.Itoa(pid) + "/cmdline"
}

// signalVMM sends one signal to a process that has already been proven to be a VMM.
//
// An ESRCH is success: the process exited between the check and the signal, which is
// the outcome the caller wanted.
func signalVMM(pid int, sig syscall.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("firecracker: find pid %d: %w", pid, err)
	}

	if err := proc.Signal(sig); err != nil {
		if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
			return nil
		}

		return fmt.Errorf("firecracker: signal pid %d: %w", pid, err)
	}

	return nil
}
