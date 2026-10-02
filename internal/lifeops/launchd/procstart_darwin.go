package launchd

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStart is when the kernel says pid started, to the microsecond: with
// the pid it names one process for its whole life, and unlike `ps -o lstart=`
// it reads the same whatever the caller's time zone and locale, which differ
// between the operator's shell and the launch agent.
func processStart(pid int) (string, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", fmt.Errorf("sysctl kern.proc.pid.%d: %w", pid, err)
	}

	if int(k.Proc.P_pid) != pid {
		return "", fmt.Errorf("sysctl kern.proc.pid.%d named pid %d", pid, k.Proc.P_pid)
	}

	start := k.Proc.P_starttime
	if start.Sec == 0 && start.Usec == 0 {
		return "", fmt.Errorf("sysctl kern.proc.pid.%d reported no start time", pid)
	}

	return fmt.Sprintf("%d.%06d", start.Sec, start.Usec), nil
}
