package main

import (
	"fmt"
	"strconv"
	"syscall"
)

// acceptanceLockFD names a descriptor the process that started `billet acceptance run`
// handed it holding a lock on the run, so the lock lasts while this process does (the
// acceptance matrix's supervisor, scripts/acceptance/matrix-supervise.py).
const acceptanceLockFD = "BILLET_ACCEPTANCE_LOCK_FD"

// keepAcceptanceLock marks the handed-down lock's descriptor close-on-exec, so the
// lock stays with this process and goes no further: `billet server` and `billet node`,
// which this run starts and may outlive it by their stop's grace, would otherwise
// inherit it, and a teardown that waits for the lock would wait on them after the run
// had ended. Unset, there is nothing to keep.
func keepAcceptanceLock(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	raw := getenv(acceptanceLockFD)
	if raw == "" {
		return nil
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return fmt.Errorf("%s=%q names no descriptor a lock could have been handed down on", acceptanceLockFD, raw)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("%s=%d is not an open descriptor: %w", acceptanceLockFD, fd, err)
	}
	syscall.CloseOnExec(fd)

	return nil
}
