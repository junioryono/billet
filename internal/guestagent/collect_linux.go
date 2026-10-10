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

// LowerPriority sets the agent to nice n. Linux keeps a nice value per thread, and
// a thread starts with its creator's, so every thread the process has is set, twice
// over, to catch one created while the first pass ran from a thread not yet set.
func LowerPriority(n int) error {
	for range 2 {
		tasks, err := os.ReadDir("/proc/self/task")
		if err != nil {
			return fmt.Errorf("list the agent's threads: %w", err)
		}

		for _, t := range tasks {
			tid, err := strconv.Atoi(t.Name())
			if err != nil {
				continue
			}

			// A thread that exited since the listing is not one to set.
			if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, n); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("set thread %d to nice %d: %w", tid, n, err)
			}
		}
	}

	return nil
}
