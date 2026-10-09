package main

import (
	"os"
	"syscall"
)

// forkSafeWriteFile writes an executable a test is about to run, under
// syscall.ForkLock: a fork by another test while the file is open for writing
// would inherit the descriptor, and exec of the file would then fail with
// ETXTBSY until that child execs (Go issue 22315).
func forkSafeWriteFile(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(name, data, perm)
}
