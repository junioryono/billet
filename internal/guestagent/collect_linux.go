//go:build linux

package guestagent

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// Supported is nil: the agent reads a Linux guest's /proc.
func Supported() error { return nil }

// maxPriorityPasses bounds the passes over the agent's threads.
const maxPriorityPasses = 16

// LowerPriority sets the agent to nice n. Linux keeps a nice value per thread, and
// a thread starts with its creator's, so the threads are listed and set until a
// pass finds none it had not set: every thread there then was set, and every one
// made since was made by one of them.
func LowerPriority(n int) error {
	set := map[int]bool{}

	for range maxPriorityPasses {
		tasks, err := os.ReadDir("/proc/self/task")
		if err != nil {
			return fmt.Errorf("list the agent's threads: %w", err)
		}

		fresh := false

		for _, t := range tasks {
			tid, err := strconv.Atoi(t.Name())
			if err != nil || set[tid] {
				continue
			}

			// A thread that exited since the listing is not one to set.
			if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, n); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("set thread %d to nice %d: %w", tid, n, err)
			}

			set[tid], fresh = true, true
		}

		if !fresh {
			return nil
		}
	}

	return fmt.Errorf("the agent's threads were still appearing after %d passes", maxPriorityPasses)
}
