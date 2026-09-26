package guestassets_test

import (
	"os"
	"syscall"
)

// forkSafeWriteFile writes an executable a test is about to run.
//
// UNDER syscall.ForkLock, which every os/exec start holds while it creates its
// child: a fork by another parallel test while this file is open for writing
// inherits the descriptor, and exec of the file then fails with ETXTBSY ("text
// file busy") until that child execs (Go issue 22315). A shell that meets it on a
// shebang interpreter reports exit 126 and fails the test for nothing.
//
//nolint:unparam // os.WriteFile's signature in every package, so a call site reads like the call it replaces
func forkSafeWriteFile(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(name, data, perm)
}
