package config

import (
	"fmt"
	"strings"
	"time"
)

// defaultDrainTimeout is when a draining billet starts SAYING the drain is long.
//
// IT IS NOT A DEADLINE, AND NOTHING ENDS WHEN IT EXPIRES. It used to bound how
// long a stopping billet waited before destroying the jobs it was still running,
// which made a timer the thing that failed somebody's build — and GitHub does not
// requeue a job whose runner vanished after starting. A job may run for days;
// elapsed time is not evidence that one stopped making progress, and billet
// imposes no job limit of its own. So a drain waits for as long as the work
// takes, and this value decides only when billet begins reporting that it is
// still waiting, and how long a CLI caller watches before it stops watching.
//
// SIX HOURS BECAUSE THAT IS HOW LONG A JOB MAY RUN: GitHub's
// jobs.<job_id>.timeout-minutes defaults to 360, so a drain shorter than that is
// unremarkable and one longer than it is worth a line in the log.
//
// Nothing is destroyed by exceeding it. Ending compute is a separately named
// operator action, never a consequence of having waited.
const defaultDrainTimeout = 6 * time.Hour

// DrainTimeoutDuration parses Server.DrainTimeout, reporting the default when unset.
//
// Parsed on demand rather than at load time, so the config type stays a plain
// data shape — but Validate calls it too, so a typo is reported when the file is
// read rather than at the shutdown that needed it.
func (s *ServerConfig) DrainTimeoutDuration() (time.Duration, error) {
	if s == nil {
		return defaultDrainTimeout, nil
	}

	return parseDrainTimeout("server.drain_timeout", s.DrainTimeout)
}

// DrainTimeoutDuration parses Node.DrainTimeout, reporting the default when unset.
//
// Separate from the server's because a node and a control plane are restarted
// for different reasons and need not wait the same amount of time.
func (n *NodeConfig) DrainTimeoutDuration() (time.Duration, error) {
	if n == nil {
		return defaultDrainTimeout, nil
	}

	return parseDrainTimeout("node.drain_timeout", n.DrainTimeout)
}

func parseDrainTimeout(key, raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return defaultDrainTimeout, nil
	}

	// The message names the shape that works. time.ParseDuration's own error for
	// a bare number is "missing unit in duration", which is true and leaves the
	// reader to guess which units exist.
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration such as \"90m\", \"6h\" or \"30s\": %w",
			key, raw, err)
	}

	// Zero is refused rather than read as "use the default". Someone who writes
	// 0s means "do not wait", and silently waiting six hours instead is the
	// divergence between the file and the behaviour that this check exists to
	// stop. Refusing it is only an obstacle unless the message says how to get
	// what they asked for, so it does.
	if d == 0 {
		return 0, fmt.Errorf(
			"%s: 0 would have billet report an overrunning drain from the instant one "+
				"begins. It no longer stops the wait — nothing does but a second signal, "+
				"and that leaves the work running — so remove the key to report after the "+
				"default %s", key, defaultDrainTimeout)
	}

	if d < 0 {
		return 0, fmt.Errorf("%s: %q must be positive", key, raw)
	}

	// NO CEILING ANY MORE. A 24h maximum was right while this bounded how long
	// billet waited before destroying work: past that magnitude a typo was
	// likelier than the intent, and believing one left a service manager waiting
	// effectively forever. It now bounds only when billet starts talking, so a
	// number too large costs a quieter log and nothing else — and refusing an
	// operator's config over that would be theatre.
	return d, nil
}
